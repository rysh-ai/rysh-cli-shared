// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Phase 2 item E — SSE streaming for the Anthropic Messages API.
//
// Streaming is exposed as a separate, optional interface (StreamingProvider)
// so existing AgenticProvider implementations remain valid. Orchestrators
// type-assert their provider against StreamingProvider to opt in.

// StreamEventType enumerates the kinds of stream events delivered to a
// StreamCallback. The set is intentionally small: a faithful low-fidelity
// mapping of Anthropic's SSE events to a single union type. Callers don't
// need to know how Anthropic frames its events; they need only what blocks
// arrived and what text/tool_use deltas accompanied them.
type StreamEventType string

const (
	StreamEventMessageStart      StreamEventType = "message_start"
	StreamEventContentBlockStart StreamEventType = "content_block_start"
	StreamEventTextDelta         StreamEventType = "text_delta"
	StreamEventToolUseDelta      StreamEventType = "tool_use_delta"
	StreamEventThinkingDelta     StreamEventType = "thinking_delta" // Phase 3 F
	StreamEventContentBlockStop  StreamEventType = "content_block_stop"
	StreamEventMessageDelta      StreamEventType = "message_delta"
	StreamEventMessageStop       StreamEventType = "message_stop"
)

// StreamEvent is the single event type delivered to a StreamCallback. Only
// the fields relevant to a given Type are populated.
type StreamEvent struct {
	Type        StreamEventType
	Index       int        // for *content_block_* events
	Text        string     // text_delta
	ToolUseID   string     // content_block_start (tool_use)
	ToolUseName string     // content_block_start (tool_use)
	PartialJSON string     // tool_use_delta
	Thinking    string     // thinking_delta (Phase 3 F)
	StopReason  StopReason // message_delta
	Usage       Usage      // message_start / message_delta
}

// StreamCallback is invoked once per parsed event. Implementations should
// return quickly; a slow callback blocks the SSE reader. Pass nil to
// stream-and-collect without intermediate updates (useful for tests).
type StreamCallback func(StreamEvent)

// StreamingProvider is the optional capability extension that signals a
// provider supports incremental token delivery. Orchestrators check via type
// assertion and fall back to CompleteWithTools when absent.
type StreamingProvider interface {
	CompleteWithToolsStream(
		ctx context.Context,
		conversation []ConversationTurn,
		tools []ToolSpec,
		systemPrompt string,
		cb StreamCallback,
	) (*AgenticResponse, error)
}

// CompleteWithToolsStream is the streaming equivalent of CompleteWithTools.
// It sets stream=true on the request body and consumes Anthropic's SSE
// response, invoking cb on each event and returning the assembled
// AgenticResponse once the stream completes.
//
// The assembled response is byte-for-byte equivalent in shape to what
// CompleteWithTools returns (same TextBlocks, ToolCalls, StopReason, Usage),
// so callers can swap the two with no downstream changes.
func (c *ClaudeAgenticProvider) CompleteWithToolsStream(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	cb StreamCallback,
) (*AgenticResponse, error) {
	// Retry policy, effort self-heal, and one-shot model fallback are shared
	// with the other entry points via runWithRetryAndFallback (Phase 3 G).
	return c.runWithRetryAndFallback(ctx, func(cc *ClaudeAgenticProvider, ctx context.Context, model string) (*AgenticResponse, retryClassification, time.Duration, error) {
		return cc.streamOnce(ctx, conversation, tools, systemPrompt, model, cb)
	})
}

// streamOnce runs one streaming attempt against the named model. Mirrors
// completeOnce but uses the SSE parser.
func (c *ClaudeAgenticProvider) streamOnce(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	model string,
	cb StreamCallback,
) (*AgenticResponse, retryClassification, time.Duration, error) {
	messages := c.buildMessages(conversation)
	toolDefs := c.buildToolDefs(tools)
	return c.doStream(ctx, c.newAgenticRequest(messages, toolDefs, systemPrompt, model, true), cb)
}

// doStream performs one streaming HTTP round trip for an assembled request
// body and consumes the SSE response. Shared transport for the shim and
// native paths.
func (c *ClaudeAgenticProvider) doStream(ctx context.Context, reqBody agenticRequest, cb StreamCallback) (*AgenticResponse, retryClassification, time.Duration, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, classFatal, 0, fmt.Errorf("claude-agentic stream: marshal request: %w", err)
	}

	endpoint := c.apiURL + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, classFatal, 0, fmt.Errorf("claude-agentic stream: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err), 0, fmt.Errorf("claude-agentic stream: request failed: %w", err)
	}
	defer httpResp.Body.Close()

	class, retryAfter := classifyHTTPError(httpResp.StatusCode, httpResp.Header)
	if class != classSuccess {
		body, _ := io.ReadAll(httpResp.Body)
		var errResp apiErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != nil {
			return nil, class, retryAfter, fmt.Errorf("claude-agentic stream: %s (HTTP %d): %s",
				errResp.Error.Type, httpResp.StatusCode, errResp.Error.Message)
		}
		return nil, class, retryAfter, fmt.Errorf("claude-agentic stream: HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	resp, parseErr := parseClaudeStream(httpResp.Body, cb)
	if parseErr != nil {
		// A truncated stream is transient (network blip); a malformed payload
		// is fatal. The current parser returns a generic error message in both
		// cases — be conservative and treat as transient so the orchestrator
		// retries, which matches the operator's intuition.
		return resp, classTransient, 0, parseErr
	}
	return resp, classSuccess, 0, nil
}

// streamEventEnvelope is the wire-level union the Anthropic SSE stream
// delivers in each `data:` payload. Fields are optional and present based on
// the value of Type.
type streamEventEnvelope struct {
	Type         string              `json:"type"`
	Index        int                 `json:"index,omitempty"`
	ContentBlock *streamContentBlock `json:"content_block,omitempty"`
	Delta        *streamDelta        `json:"delta,omitempty"`
	Message      *streamMessage      `json:"message,omitempty"`
	Usage        *agenticUsage       `json:"usage,omitempty"`
}

type streamContentBlock struct {
	Type  string          `json:"type"` // "text", "tool_use", "thinking"
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type streamDelta struct {
	Type        string `json:"type"` // "text_delta", "input_json_delta", "thinking_delta", "signature_delta" (content_block_delta); blank on message_delta
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"` // present on message_delta
	Thinking    string `json:"thinking,omitempty"`    // Phase 3 F: thinking_delta payload
	Signature   string `json:"signature,omitempty"`   // Phase 3 F: signature_delta payload (block provenance)
}

type streamMessage struct {
	ID         string        `json:"id,omitempty"`
	Model      string        `json:"model,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
	Usage      *agenticUsage `json:"usage,omitempty"`
}

// streamBlockAcc accumulates per-content-block state as deltas arrive.
type streamBlockAcc struct {
	kind      string // "text" | "tool_use" | "thinking"
	text      strings.Builder
	toolID    string
	toolName  string
	input     strings.Builder
	thinking  strings.Builder // Phase 3 F: accumulated thinking text
	signature string          // Phase 3 F: thinking block signature
}

// parseClaudeStream reads an Anthropic SSE stream from r, dispatching each
// event to cb (if non-nil) and returning the assembled AgenticResponse on
// message_stop. Unparseable `data:` lines and unknown event types are
// skipped silently — the stream protocol may evolve, and downgrading
// gracefully is preferable to aborting.
func parseClaudeStream(r io.Reader, cb StreamCallback) (*AgenticResponse, error) {
	scanner := bufio.NewScanner(r)
	// SSE lines for tool_use input_json_delta can be long (tens of KB on
	// large tool inputs). Allocate a generous max-line budget.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	blocks := map[int]*streamBlockAcc{}
	response := &AgenticResponse{}
	safe := func(ev StreamEvent) {
		if cb != nil {
			cb(ev)
		}
	}
	finalize := func() *AgenticResponse {
		indices := make([]int, 0, len(blocks))
		for i := range blocks {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			acc := blocks[i]
			switch acc.kind {
			case "text":
				if acc.text.Len() > 0 {
					response.TextBlocks = append(response.TextBlocks, TextBlock{Text: acc.text.String()})
				}
			case "tool_use":
				input := strings.TrimSpace(acc.input.String())
				if input == "" {
					input = "{}"
				}
				// A stream that terminates mid input_json_delta (cancellation,
				// network drop) leaves TRUNCATED JSON in the accumulator. If
				// that ever reaches a conversation turn it poisons every
				// subsequent request at marshal time ("unexpected end of JSON
				// input"), so fall back to the empty object. This only fires on
				// the partial-result path — a complete stream always carries
				// valid input by message_stop.
				if !json.Valid([]byte(input)) {
					input = "{}"
				}
				response.ToolCalls = append(response.ToolCalls, ToolCallRequest{
					ID:    acc.toolID,
					Name:  acc.toolName,
					Input: json.RawMessage(input),
				})
			case "thinking":
				// Phase 3 F: thinking blocks may stream as multiple
				// thinking_delta chunks; accumulate and emit as a single
				// ThinkingBlock with the final signature.
				if acc.thinking.Len() > 0 {
					response.ThinkingBlocks = append(response.ThinkingBlocks, ThinkingBlock{
						Text:      acc.thinking.String(),
						Signature: acc.signature,
					})
				}
			}
		}
		return response
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if len(payload) == 0 {
			continue
		}
		var env streamEventEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			// Malformed events should not abort the stream — log-and-skip.
			continue
		}

		switch env.Type {
		case "message_start":
			if env.Message != nil && env.Message.Usage != nil {
				response.Usage.InputTokens = env.Message.Usage.InputTokens
				response.Usage.OutputTokens = env.Message.Usage.OutputTokens
				response.Usage.CacheReadInputTokens = env.Message.Usage.CacheReadInputTokens
				response.Usage.CacheCreationInputTokens = env.Message.Usage.CacheCreationInputTokens
			}
			safe(StreamEvent{Type: StreamEventMessageStart, Usage: response.Usage})

		case "content_block_start":
			if env.ContentBlock == nil {
				continue
			}
			acc := &streamBlockAcc{kind: env.ContentBlock.Type}
			if env.ContentBlock.Type == "tool_use" {
				acc.toolID = env.ContentBlock.ID
				acc.toolName = env.ContentBlock.Name
				// The Anthropic streaming API sends an empty placeholder
				// ("input": {}) on content_block_start for tool_use blocks;
				// the real arguments arrive separately via input_json_delta
				// events. Seeding the accumulator with that "{}" placeholder
				// and then appending the partial-JSON deltas yields invalid
				// concatenated JSON like `{}{"command":"pwd"}`, which later
				// breaks both tool param parsing and re-marshalling the
				// assistant turn (json.RawMessage rejects "invalid character
				// '{' after top-level value"). So only seed when the start
				// block carries a genuine non-empty object — never expected
				// on a stream, but defensive against future shapes.
				if seed := bytes.TrimSpace(env.ContentBlock.Input); len(seed) > 0 && !bytes.Equal(seed, []byte("{}")) {
					acc.input.Write(seed)
				}
				safe(StreamEvent{
					Type:        StreamEventContentBlockStart,
					Index:       env.Index,
					ToolUseID:   acc.toolID,
					ToolUseName: acc.toolName,
				})
			} else {
				safe(StreamEvent{Type: StreamEventContentBlockStart, Index: env.Index})
			}
			blocks[env.Index] = acc

		case "content_block_delta":
			acc := blocks[env.Index]
			if acc == nil || env.Delta == nil {
				continue
			}
			switch env.Delta.Type {
			case "text_delta":
				acc.text.WriteString(env.Delta.Text)
				safe(StreamEvent{Type: StreamEventTextDelta, Index: env.Index, Text: env.Delta.Text})
			case "input_json_delta":
				acc.input.WriteString(env.Delta.PartialJSON)
				safe(StreamEvent{Type: StreamEventToolUseDelta, Index: env.Index, PartialJSON: env.Delta.PartialJSON})
			case "thinking_delta":
				// Phase 3 F.
				acc.thinking.WriteString(env.Delta.Thinking)
				safe(StreamEvent{Type: StreamEventThinkingDelta, Index: env.Index, Thinking: env.Delta.Thinking})
			case "signature_delta":
				// Phase 3 F: the API streams the block signature separately
				// from the thinking text. Always last (replaces, not appends).
				acc.signature = env.Delta.Signature
			}

		case "content_block_stop":
			safe(StreamEvent{Type: StreamEventContentBlockStop, Index: env.Index})

		case "message_delta":
			if env.Delta != nil && env.Delta.StopReason != "" {
				response.StopReason = StopReason(env.Delta.StopReason)
			}
			if env.Usage != nil && env.Usage.OutputTokens > 0 {
				response.Usage.OutputTokens = env.Usage.OutputTokens
			}
			safe(StreamEvent{Type: StreamEventMessageDelta, StopReason: response.StopReason, Usage: response.Usage})

		case "message_stop":
			safe(StreamEvent{Type: StreamEventMessageStop})
			return finalize(), nil

		case "ping":
			// Heartbeat; ignore.

		default:
			// Unknown event type; ignore for forward compatibility.
		}
	}
	if err := scanner.Err(); err != nil {
		return finalize(), fmt.Errorf("claude-agentic stream: read: %w", err)
	}
	// Stream ended without message_stop. Return what we have along with a
	// non-fatal error so callers can surface the partial result.
	return finalize(), errors.New("claude-agentic stream: ended without message_stop")
}
