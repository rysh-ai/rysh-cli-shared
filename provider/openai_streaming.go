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
)

// Design 002 — SSE streaming for OpenAI-compatible Chat Completions endpoints.
// Serves openai, ollama, and gemini (generativelanguage .../v1beta/openai).
//
// This closes the streaming gap called out in openai_provider.go: the
// orchestrator type-asserts StreamingProvider and previously fell back to
// CompleteWithTools for these providers. The contract mirrors the Claude
// implementation in streaming.go exactly — cb receives the same StreamEvent
// union, and the assembled AgenticResponse is shape-equivalent to what
// CompleteWithTools returns (same TextBlocks, ToolCalls, StopReason, Usage) —
// so the orchestrator needs no changes.
//
// Wire format (chat.completion.chunk): each SSE `data:` line carries a JSON
// chunk whose choices[0].delta holds a content fragment and/or tool_call
// fragments. Tool-call fragments are keyed by their own `index`; id and
// function.name arrive on the first fragment and function.arguments
// accumulates across fragments. The stream terminates with `data: [DONE]`.
// Usage arrives only when the request sets stream_options.include_usage —
// typically as a final chunk with empty choices — and some servers (older
// Ollama) never send it, which callers must tolerate.

// Interface guard: the OpenAI-compatible provider now streams, so structural
// capability detection (rysh-cli Caps()) reports Streaming=true.
var _ StreamingProvider = (*OpenAIAgenticProvider)(nil)

// CompleteWithToolsStream is the streaming equivalent of CompleteWithTools,
// and routes by dialect the same way it does: OpenAI proper streams from the
// Responses API (openai_responses_streaming.go), every other
// OpenAI-compatible endpoint streams Chat Completions from here. It sets
// stream=true (plus stream_options.include_usage) on the request body and
// consumes the SSE response, invoking cb on each event and returning the
// assembled AgenticResponse once the stream completes.
//
// Setup failures degrade gracefully, in two steps. An HTTP 400 is the
// signature of an OpenAI-compatible server rejecting the payload, but the
// usual culprit is stream_options alone — older Ollama builds and strict
// proxies 400 on the field while streaming itself works. Abandoning streaming
// on that first 400 would silently cost air-gapped users (design 002's whole
// point) their incremental output, so retry once WITHOUT stream_options and
// only fall through to the non-streaming path if that is refused too. A
// genuinely bad payload still surfaces the same error CompleteWithTools would
// return. Other statuses return to the orchestrator's retry loop: re-sending
// an unstreamed 401 or 429 fails identically.
func (c *OpenAIAgenticProvider) CompleteWithToolsStream(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	cb StreamCallback,
) (*AgenticResponse, error) {
	return c.withStreamRetry(ctx, cb, func(ctx context.Context, cb StreamCallback) (*AgenticResponse, error) {
		if c.usesResponsesAPI() {
			return c.doResponsesStream(ctx, c.buildResponsesRequest(conversation, tools, systemPrompt), cb, func() (*AgenticResponse, error) {
				return c.doResponses(ctx, c.buildResponsesRequest(conversation, tools, systemPrompt))
			})
		}
		return c.doStream(ctx, c.buildRequest(conversation, tools, systemPrompt), cb, func() (*AgenticResponse, error) {
			return c.doComplete(ctx, c.buildRequest(conversation, tools, systemPrompt))
		})
	})
}

// errStreamRejected marks an HTTP 400 from the streaming endpoint, the signal
// doStream uses to step down to the next degrade stage.
var errStreamRejected = errors.New("streaming request rejected")

// doStream drives the staged-degrade streaming call for an assembled request
// body, shared by the shim path (CompleteWithToolsStream) and the native
// ChatProvider path (ChatStream): stream with usage → stream without
// stream_options (older Ollama builds and strict proxies 400 on the field
// while streaming itself works; usage then goes unreported, tolerated by
// parseOpenAIStream) → the caller's equivalent non-streaming call. A genuinely
// bad payload fails the non-streaming call identically, so its error still
// surfaces; other statuses return to the orchestrator's retry loop unstaged.
func (c *OpenAIAgenticProvider) doStream(ctx context.Context, reqBody oaiRequest, cb StreamCallback, nonStreaming func() (*AgenticResponse, error)) (*AgenticResponse, error) {
	resp, err := c.streamOnce(ctx, reqBody, true, cb)
	if !errors.Is(err, errStreamRejected) {
		return resp, err
	}
	resp, err = c.streamOnce(ctx, reqBody, false, cb)
	if !errors.Is(err, errStreamRejected) {
		return resp, err
	}
	// Streaming itself is refused: complete the turn unstreamed.
	return nonStreaming()
}

// streamOnce performs one streaming HTTP round trip (Stream/StreamOptions are
// set here). includeUsage controls whether stream_options is sent; an HTTP 400
// is reported as errStreamRejected so doStream can decide how far to degrade,
// with the response body preserved so a genuinely bad payload stays reportable.
func (c *OpenAIAgenticProvider) streamOnce(ctx context.Context, reqBody oaiRequest, includeUsage bool, cb StreamCallback) (*AgenticResponse, error) {
	reqBody.Stream = true
	if includeUsage {
		// Without this OpenAI omits usage from the stream entirely.
		reqBody.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	} else {
		reqBody.StreamOptions = nil
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: stream request: %w", c.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusBadRequest {
			// Let doStream step down a degrade stage; the body is preserved
			// so a genuinely bad payload is still reportable.
			return nil, fmt.Errorf("%s: stream status 400: %s: %w",
				c.name, strings.TrimSpace(string(respBody)), errStreamRejected)
		}
		return nil, newOpenAIHTTPError(c.name, "stream status", resp.StatusCode, resp.Header, string(respBody))
	}

	return parseOpenAIStream(resp.Body, c.name, cb)
}

// ---- OpenAI stream wire shapes ----------------------------------------------

// oaiStreamChunk is one `data:` payload (object "chat.completion.chunk").
type oaiStreamChunk struct {
	Choices []struct {
		Index        int            `json:"index"`
		Delta        oaiStreamDelta `json:"delta"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"` // only with stream_options.include_usage; absent on older Ollama
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type oaiStreamDelta struct {
	Content   string               `json:"content"`
	ToolCalls []oaiStreamToolDelta `json:"tool_calls"`
}

// oaiStreamToolDelta is one tool_call fragment. Index keys the accumulator;
// id/name/arguments are partial and concatenate across fragments.
type oaiStreamToolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaiToolAcc accumulates one tool call's fragments as they arrive.
type oaiToolAcc struct {
	id    strings.Builder
	name  strings.Builder
	args  strings.Builder
	block int // callback block index (shared sequence with the text block)
}

// parseOpenAIStream reads an OpenAI-compatible SSE stream from r, dispatching
// each event to cb (if non-nil) and returning the assembled AgenticResponse on
// [DONE]. Unparseable `data:` lines are skipped silently, mirroring
// parseClaudeStream — degrading gracefully beats aborting.
//
// Callback block indices are synthesized (OpenAI has no content-block framing):
// the text block and each tool call get sequential indices in arrival order,
// so consumers see the same start/delta/stop shape Claude streams produce.
func parseOpenAIStream(r io.Reader, name string, cb StreamCallback) (*AgenticResponse, error) {
	scanner := bufio.NewScanner(r)
	// Tool-call argument fragments can be large on big tool inputs; match the
	// Claude parser's max-line budget.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	response := &AgenticResponse{}
	safe := func(ev StreamEvent) {
		if cb != nil {
			cb(ev)
		}
	}

	var (
		text      strings.Builder
		textBlock = -1 // callback index once text starts
		toolAccs  = map[int]*oaiToolAcc{}
		nextBlock int
		started   bool
		sawDone   bool
	)

	finalize := func() *AgenticResponse {
		if text.Len() > 0 {
			response.TextBlocks = append(response.TextBlocks, TextBlock{Text: text.String()})
		}
		indices := make([]int, 0, len(toolAccs))
		for i := range toolAccs {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			acc := toolAccs[i]
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			// A stream that dies mid-arguments leaves truncated JSON in the
			// accumulator; letting it reach a conversation turn poisons every
			// subsequent request at marshal time (see the equivalent guard in
			// parseClaudeStream). Fall back to the empty object.
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			response.ToolCalls = append(response.ToolCalls, ToolCallRequest{
				ID:    acc.id.String(),
				Name:  acc.name.String(),
				Input: json.RawMessage(args),
			})
		}
		// Some OpenAI-compatible servers end the stream without a
		// finish_reason. Default it the way mapFinishReason would, so the
		// orchestrator's tool-dispatch check still fires.
		if response.StopReason == "" {
			if len(response.ToolCalls) > 0 {
				response.StopReason = StopReasonToolUse
			} else {
				response.StopReason = StopReasonEndTurn
			}
		}
		return response
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			safe(StreamEvent{Type: StreamEventMessageStop})
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// Malformed events should not abort the stream — skip.
			continue
		}
		if chunk.Error != nil {
			return finalize(), fmt.Errorf("%s: stream: %s", name, chunk.Error.Message)
		}
		if !started {
			started = true
			safe(StreamEvent{Type: StreamEventMessageStart})
		}
		if chunk.Usage != nil {
			// Usually the final chunk (empty choices) when include_usage is
			// set; absent entirely on servers that never send usage.
			response.Usage.InputTokens = chunk.Usage.PromptTokens
			response.Usage.OutputTokens = chunk.Usage.CompletionTokens
			safe(StreamEvent{Type: StreamEventMessageDelta, StopReason: response.StopReason, Usage: response.Usage})
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0] // n=1 always; ignore hypothetical extras

		if choice.Delta.Content != "" {
			if textBlock < 0 {
				textBlock = nextBlock
				nextBlock++
				safe(StreamEvent{Type: StreamEventContentBlockStart, Index: textBlock})
			}
			text.WriteString(choice.Delta.Content)
			safe(StreamEvent{Type: StreamEventTextDelta, Index: textBlock, Text: choice.Delta.Content})
		}

		for _, td := range choice.Delta.ToolCalls {
			acc, ok := toolAccs[td.Index]
			if !ok {
				acc = &oaiToolAcc{block: nextBlock}
				nextBlock++
				toolAccs[td.Index] = acc
			}
			acc.id.WriteString(td.ID)
			acc.name.WriteString(td.Function.Name)
			if !ok {
				// First fragment carries id+name on every known server, so the
				// start event is as informative as Claude's.
				safe(StreamEvent{
					Type:        StreamEventContentBlockStart,
					Index:       acc.block,
					ToolUseID:   acc.id.String(),
					ToolUseName: acc.name.String(),
				})
			}
			if td.Function.Arguments != "" {
				acc.args.WriteString(td.Function.Arguments)
				safe(StreamEvent{Type: StreamEventToolUseDelta, Index: acc.block, PartialJSON: td.Function.Arguments})
			}
		}

		if choice.FinishReason != "" {
			response.StopReason = mapFinishReason(choice.FinishReason)
			// OpenAI has no per-block stop framing; close every open block now.
			if textBlock >= 0 {
				safe(StreamEvent{Type: StreamEventContentBlockStop, Index: textBlock})
			}
			toolIdx := make([]int, 0, len(toolAccs))
			for i := range toolAccs {
				toolIdx = append(toolIdx, i)
			}
			sort.Ints(toolIdx)
			for _, i := range toolIdx {
				safe(StreamEvent{Type: StreamEventContentBlockStop, Index: toolAccs[i].block})
			}
			safe(StreamEvent{Type: StreamEventMessageDelta, StopReason: response.StopReason, Usage: response.Usage})
		}
	}

	if err := scanner.Err(); err != nil {
		return finalize(), fmt.Errorf("%s: stream read: %w", name, err)
	}
	if !sawDone {
		// Stream ended without [DONE]. Return what we have along with a
		// non-fatal error so callers can surface the partial result.
		return finalize(), fmt.Errorf("%s: stream ended without [DONE]", name)
	}
	return finalize(), nil
}
