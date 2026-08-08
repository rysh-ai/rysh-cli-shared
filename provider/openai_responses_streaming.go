package provider

// openai_responses_streaming.go — SSE streaming for the OpenAI Responses API.
//
// The non-streaming dialect lives in openai_responses.go; this is its
// streaming half. Without it the GPT-5.6 tier would still 400 on every turn,
// because the streaming path would keep posting Chat Completions bodies to an
// endpoint that refuses function tools there.
//
// The wire format was verified against the live API (the published docs 403).
// It is NOT Chat Completions with different field names — it is a different
// shape of stream:
//
//	Chat Completions                      Responses
//	one chunk object per event            one TYPED event per `data:` line
//	choices[0].delta.content              response.output_text.delta .delta
//	choices[0].delta.tool_calls[]         response.function_call_arguments.delta
//	  (id/name on the first fragment)       (id/name on output_item.added)
//	tool fragments keyed by delta index   items keyed by output_index
//	finish_reason on the last chunk       response.completed / .incomplete
//	usage via stream_options              usage on the terminal event, always
//	terminates with `data: [DONE]`        terminates with the terminal event
//
// Two consequences worth stating outright. There is NO [DONE] sentinel, so the
// terminal response.* event is what ends the stream — treating its absence the
// way the Chat Completions parser treats a missing [DONE] is what makes a
// dropped connection reportable. And the terminal event carries the entire
// finished response object, so the assembled result is taken from the server's
// own final view rather than from replayed deltas; the delta accumulators
// exist for the truncated case, where there is no terminal event to trust.
//
// Callers see the same StreamEvent sequence every other provider emits
// (message_start → content_block_start → deltas → content_block_stop →
// message_delta → message_stop), so the orchestrator needs no changes.

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

// ---------------------------------------------------------------------------
// Stream wire shapes
// ---------------------------------------------------------------------------

// respStreamEvent is one `data:` payload. The stream is a union discriminated
// by Type; each variant populates a different subset of these fields. (The
// companion `event:` line carries the same string, so it is ignored.)
type respStreamEvent struct {
	Type string `json:"type"`
	// OutputIndex keys the output item this event belongs to — the equivalent
	// of Chat Completions' tool-call fragment index, but shared by message and
	// function_call items alike.
	OutputIndex int `json:"output_index"`
	// Delta is the incremental payload for output_text.delta (text) and
	// function_call_arguments.delta (raw JSON fragment).
	Delta string `json:"delta"`
	// Item is present on output_item.added / .done and identifies what the
	// item IS — including a function call's id and name, which arrive here
	// rather than on the argument fragments.
	Item *struct {
		Type   string `json:"type"` // "message" | "function_call" | "reasoning"
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	} `json:"item"`
	// Response is the complete response object, present on the terminal
	// events (completed / incomplete / failed).
	Response *respResponse `json:"response"`
	// Error is set on a top-level `error` event.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// respBlockAcc accumulates one output item's deltas, so a stream that dies
// before the terminal event still yields what arrived.
type respBlockAcc struct {
	block  int // callback block index
	isTool bool
	id     string
	name   string
	buf    strings.Builder // text, or raw argument JSON
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// doResponsesStream drives the streaming call with a one-step degrade: stream
// → the caller's equivalent non-streaming call if the server refuses to stream
// at all. There is no middle stage here (the Chat Completions ladder's extra
// stage exists only to retry without stream_options, which this dialect has
// no equivalent of). A genuinely bad payload fails the non-streaming call
// identically, so its error still surfaces; other statuses return to the
// orchestrator's retry loop unstaged.
func (c *OpenAIAgenticProvider) doResponsesStream(
	ctx context.Context,
	reqBody respRequest,
	cb StreamCallback,
	nonStreaming func() (*AgenticResponse, error),
) (*AgenticResponse, error) {
	resp, err := c.responsesStreamOnce(ctx, reqBody, cb)
	if !errors.Is(err, errStreamRejected) {
		return resp, err
	}
	return nonStreaming()
}

// responsesStreamOnce performs one streaming round trip against /responses.
// An HTTP 400 is reported as errStreamRejected so doResponsesStream can
// degrade, with the response body preserved so a genuinely bad payload stays
// reportable.
func (c *OpenAIAgenticProvider) responsesStreamOnce(ctx context.Context, reqBody respRequest, cb StreamCallback) (*AgenticResponse, error) {
	reqBody.Stream = true

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/responses", bytes.NewReader(body))
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
			return nil, fmt.Errorf("%s: stream status 400: %s: %w",
				c.name, strings.TrimSpace(string(respBody)), errStreamRejected)
		}
		return nil, newOpenAIHTTPError(c.name, "stream status", resp.StatusCode, resp.Header, string(respBody))
	}

	return parseResponsesStream(resp.Body, c.name, cb)
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// parseResponsesStream reads a Responses SSE stream from r, dispatching each
// event to cb (if non-nil) and returning the assembled AgenticResponse once a
// terminal event arrives. Unparseable `data:` lines are skipped silently,
// mirroring parseOpenAIStream and parseClaudeStream — degrading gracefully
// beats aborting.
//
// Callback block indices are synthesized in arrival order, as on the Chat
// Completions path: reasoning items are skipped entirely (they carry no
// client-visible content, exactly as responsesToAgentic skips them), so the
// indices consumers see are dense over the blocks they actually receive.
func parseResponsesStream(r io.Reader, name string, cb StreamCallback) (*AgenticResponse, error) {
	scanner := bufio.NewScanner(r)
	// Tool-call argument fragments can be large on big tool inputs; match the
	// other parsers' max-line budget.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	safe := func(ev StreamEvent) {
		if cb != nil {
			cb(ev)
		}
	}

	var (
		accs      = map[int]*respBlockAcc{} // by output_index
		nextBlock int
		started   bool
	)

	// finalize assembles a response from the accumulated deltas. It is only
	// reached when NO terminal event arrived (a dropped connection): a
	// completed stream is assembled from the server's own final object
	// instead, which is authoritative about usage and stop reason.
	finalize := func() *AgenticResponse {
		ar := &AgenticResponse{StopReason: StopReasonEndTurn}
		indices := make([]int, 0, len(accs))
		for i := range accs {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			acc := accs[i]
			if !acc.isTool {
				if acc.buf.Len() > 0 {
					ar.TextBlocks = append(ar.TextBlocks, TextBlock{Text: acc.buf.String()})
				}
				continue
			}
			args := strings.TrimSpace(acc.buf.String())
			// A stream that dies mid-arguments leaves truncated JSON in the
			// accumulator; letting it reach a conversation turn poisons every
			// subsequent request at marshal time (same guard as
			// parseOpenAIStream). Fall back to the empty object.
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			ar.ToolCalls = append(ar.ToolCalls, ToolCallRequest{
				ID: acc.id, Name: acc.name, Input: json.RawMessage(args),
			})
		}
		if len(ar.ToolCalls) > 0 {
			ar.StopReason = StopReasonToolUse
		}
		return ar
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // `event:` lines restate Type; blank lines separate events
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 {
			continue
		}

		var ev respStreamEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue // malformed events should not abort the stream
		}
		if !started {
			started = true
			safe(StreamEvent{Type: StreamEventMessageStart})
		}

		switch ev.Type {
		case "error":
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return finalize(), fmt.Errorf("%s: stream: %s", name, msg)

		case "response.output_item.added":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "message":
				acc := &respBlockAcc{block: nextBlock}
				nextBlock++
				accs[ev.OutputIndex] = acc
				safe(StreamEvent{Type: StreamEventContentBlockStart, Index: acc.block})
			case "function_call":
				acc := &respBlockAcc{block: nextBlock, isTool: true, id: ev.Item.CallID, name: ev.Item.Name}
				nextBlock++
				accs[ev.OutputIndex] = acc
				safe(StreamEvent{
					Type:        StreamEventContentBlockStart,
					Index:       acc.block,
					ToolUseID:   acc.id,
					ToolUseName: acc.name,
				})
			}
			// "reasoning" (and any future item type) allocates no block.

		case "response.output_text.delta":
			acc, ok := accs[ev.OutputIndex]
			if !ok || ev.Delta == "" {
				continue
			}
			acc.buf.WriteString(ev.Delta)
			safe(StreamEvent{Type: StreamEventTextDelta, Index: acc.block, Text: ev.Delta})

		case "response.function_call_arguments.delta":
			acc, ok := accs[ev.OutputIndex]
			if !ok || ev.Delta == "" {
				continue
			}
			acc.buf.WriteString(ev.Delta)
			safe(StreamEvent{Type: StreamEventToolUseDelta, Index: acc.block, PartialJSON: ev.Delta})

		case "response.output_item.done":
			// Per-item stop framing, unlike Chat Completions where every open
			// block closes at once on finish_reason.
			if acc, ok := accs[ev.OutputIndex]; ok {
				safe(StreamEvent{Type: StreamEventContentBlockStop, Index: acc.block})
			}

		case "response.completed", "response.incomplete", "response.failed":
			if ev.Response == nil {
				return finalize(), fmt.Errorf("%s: stream: terminal event %q carried no response", name, ev.Type)
			}
			ar := responsesToAgentic(ev.Response)
			safe(StreamEvent{Type: StreamEventMessageDelta, StopReason: ar.StopReason, Usage: ar.Usage})
			safe(StreamEvent{Type: StreamEventMessageStop})
			if ev.Response.Error != nil {
				return ar, fmt.Errorf("%s: %s", name, ev.Response.Error.Message)
			}
			if ev.Type == "response.failed" {
				return ar, fmt.Errorf("%s: stream: response failed", name)
			}
			return ar, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return finalize(), fmt.Errorf("%s: stream read: %w", name, err)
	}
	// The stream ended without a terminal event — a dropped connection.
	// Return what arrived along with a non-fatal error, as parseOpenAIStream
	// does for a missing [DONE].
	return finalize(), fmt.Errorf("%s: stream ended without a terminal response event", name)
}
