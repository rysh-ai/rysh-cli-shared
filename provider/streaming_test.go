// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// fakeSSE builds a syntactically-correct Anthropic SSE response body from a
// list of (event, json-payload) pairs. The test uses string concatenation
// rather than the real types to keep the wire format readable inline.
func fakeSSE(events ...[2]string) string {
	var sb strings.Builder
	for _, ev := range events {
		sb.WriteString("event: ")
		sb.WriteString(ev[0])
		sb.WriteString("\ndata: ")
		sb.WriteString(ev[1])
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// TestParseClaudeStream_TextOnly exercises the happy path with a single text
// content block streamed in three deltas, asserting that the assembled
// response contains the full text and that the callback received the deltas
// in order.
func TestParseClaudeStream_TextOnly(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_x","usage":{"input_tokens":42,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", "}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)

	var deltas []string
	resp, err := parseClaudeStream(strings.NewReader(body), func(ev StreamEvent) {
		if ev.Type == StreamEventTextDelta {
			deltas = append(deltas, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Hello, world" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 17 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got := strings.Join(deltas, "|"); got != "Hello|, |world" {
		t.Errorf("deltas = %q", got)
	}
}

// TestParseClaudeStream_ToolUse exercises a tool_use block streamed in
// fragmented input_json_delta chunks, asserting reassembly into a single
// ToolCallRequest with valid JSON input.
func TestParseClaudeStream_ToolUse(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_y","usage":{"input_tokens":100,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" \"ls -la\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "bash" || tc.ID != "toolu_1" {
		t.Errorf("tool call id/name = %q/%q", tc.ID, tc.Name)
	}
	// content_block_start carries an empty "{}" placeholder; the real input
	// arrives via input_json_delta. The accumulator must NOT prepend that
	// placeholder, or the assembled input becomes invalid concatenated JSON
	// (`{}{"command": "ls -la"}`), which breaks tool parsing and request
	// re-marshalling. Assert the reassembled input is exactly the deltas and
	// is valid JSON.
	if !json.Valid(tc.Input) {
		t.Errorf("tool input is not valid JSON: %s", string(tc.Input))
	}
	if got := string(tc.Input); got != `{"command": "ls -la"}` {
		t.Errorf("tool input = %q, want %q", got, `{"command": "ls -la"}`)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
}

// TestParseClaudeStream_ToolUseEmptyPlaceholderNotPrepended is a focused
// regression test for the doubled-JSON bug: Anthropic sends `"input": {}` on
// content_block_start and then streams the real arguments via
// input_json_delta. The parser must drop the placeholder so the assembled
// input is valid JSON. Before the fix, the input was `{}{"path":"/tmp"}`,
// which made json.RawMessage.MarshalJSON fail with
// "invalid character '{' after top-level value" on the next request.
func TestParseClaudeStream_ToolUseEmptyPlaceholderNotPrepended(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_z","usage":{"input_tokens":5,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"file_read","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"/tmp\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if !json.Valid(tc.Input) {
		t.Fatalf("tool input is not valid JSON: %s", string(tc.Input))
	}
	if got := string(tc.Input); got != `{"path":"/tmp"}` {
		t.Errorf("tool input = %q, want %q", got, `{"path":"/tmp"}`)
	}
	// The bug also surfaced when the assistant turn was re-marshalled into the
	// next request. Simulate that round-trip and confirm it no longer fails.
	if _, mErr := json.Marshal(tc.Input); mErr != nil {
		t.Errorf("re-marshal of tool input failed: %v", mErr)
	}
}

// TestParseClaudeStream_ToolUseNoDeltas covers a tool_use block whose only
// payload is the empty "{}" placeholder with no input_json_delta events
// (a zero-argument tool call). The assembled input must fall back to a valid
// empty object, not an empty string.
func TestParseClaudeStream_ToolUseNoDeltas(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"git_status","input":{}}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %+v", resp.ToolCalls)
	}
	if got := string(resp.ToolCalls[0].Input); got != "{}" {
		t.Errorf("tool input = %q, want %q", got, "{}")
	}
	if !json.Valid(resp.ToolCalls[0].Input) {
		t.Errorf("tool input is not valid JSON: %s", string(resp.ToolCalls[0].Input))
	}
}

// TestParseClaudeStream_MixedBlocks asserts the assembler preserves
// block ORDERING by index when text and tool_use blocks are interleaved.
func TestParseClaudeStream_MixedBlocks(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Working on it"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"grep"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\"foo\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Working on it" {
		t.Errorf("text blocks: %+v", resp.TextBlocks)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "grep" {
		t.Errorf("tool calls: %+v", resp.ToolCalls)
	}
}

// TestParseClaudeStream_IgnoresUnknownAndPing covers the forward-compat path:
// unknown event types and SSE ping events should not abort the stream.
func TestParseClaudeStream_IgnoresUnknownAndPing(t *testing.T) {
	body := fakeSSE(
		[2]string{"ping", `{"type":"ping"}`},
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`},
		[2]string{"some_new_event", `{"type":"some_new_event","payload":42}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "ok" {
		t.Errorf("text blocks: %+v", resp.TextBlocks)
	}
}

// TestParseClaudeStream_TruncatedStream simulates a connection drop after
// message_delta but before message_stop. The parser should return what it
// has plus a non-nil error so the caller can surface the partial result.
func TestParseClaudeStream_TruncatedStream(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`},
		// no content_block_stop, no message_stop
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err == nil {
		t.Errorf("expected truncated-stream error, got nil")
	}
	if resp == nil || len(resp.TextBlocks) == 0 || resp.TextBlocks[0].Text != "partial" {
		t.Errorf("expected partial 'partial' text, got %+v", resp)
	}
}

// TestParseClaudeStream_TruncatedToolInputFallsBackToEmptyObject is a focused
// regression test for the marshal-poisoning bug: a stream that dies mid
// input_json_delta leaves truncated JSON (e.g. `{"action":"navi`) in the
// accumulator. Before the fix, finalize() passed it through verbatim; once
// recorded on an assistant turn it made EVERY subsequent request fail with
// `marshal request: json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input`. The parser must fall back to a valid empty
// object for the partial tool call.
func TestParseClaudeStream_TruncatedToolInputFallsBackToEmptyObject(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":2}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_t","name":"browser_action","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"action\":\"navi"}}`},
		// connection drops here: no more deltas, no content_block_stop, no message_stop
	)
	resp, err := parseClaudeStream(strings.NewReader(body), nil)
	if err == nil {
		t.Errorf("expected truncated-stream error, got nil")
	}
	if resp == nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 partial tool call, got %+v", resp)
	}
	tc := resp.ToolCalls[0]
	if !json.Valid(tc.Input) {
		t.Fatalf("tool input is not valid JSON: %q", string(tc.Input))
	}
	if got := string(tc.Input); got != `{}` {
		t.Errorf("tool input = %q, want %q", got, `{}`)
	}
	if _, mErr := json.Marshal(tc.Input); mErr != nil {
		t.Errorf("re-marshal of tool input failed: %v", mErr)
	}
}
