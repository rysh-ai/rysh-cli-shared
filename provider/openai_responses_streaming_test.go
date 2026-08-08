package provider

// Tests for the Responses SSE parser (openai_responses_streaming.go). The
// fixtures below are the shapes the live API actually emits — typed events,
// per-item framing, usage on the terminal event, and NO [DONE] sentinel.
//
// What matters to consumers is that this dialect produces the same StreamEvent
// sequence as every other provider, since the orchestrator and the TUI key off
// that union and not off the wire format.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeResponsesSSE builds a Responses SSE body. Each payload gets BOTH the
// `event:` line the live API sends and the `data:` line — the parser must key
// off the payload's own type field and ignore the former.
func fakeResponsesSSE(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(p), &probe)
		sb.WriteString("event: " + probe.Type + "\n")
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

// responsesStreamServer serves body for POST /responses and captures the
// decoded request.
func responsesStreamServer(t *testing.T, gotReq *respRequest, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if gotReq != nil {
			_ = json.Unmarshal(raw, gotReq)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
}

// happyResponsesStream is a full turn: a reasoning item (no client-visible
// content), a streamed text message, then a tool call whose arguments arrive
// in fragments.
func happyResponsesStream() string {
	return fakeResponsesSSE(
		`{"type":"response.created","sequence_number":0}`,
		`{"type":"response.in_progress","sequence_number":1}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":1,"content_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"msg_1","delta":"Hel"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"item_id":"msg_1","delta":"lo"}`,
		`{"type":"response.output_text.done","output_index":1,"content_index":0,"item_id":"msg_1","text":"Hello"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_a","name":"bash","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"command\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"\"ls\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":2,"item_id":"fc_1","arguments":"{\"command\":\"ls\"}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_a","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[`+
			`{"id":"rs_1","type":"reasoning","summary":[]},`+
			`{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hello"}]},`+
			`{"id":"fc_1","type":"function_call","call_id":"call_a","name":"bash","arguments":"{\"command\":\"ls\"}"}],`+
			`"usage":{"input_tokens":42,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":1},"output_tokens":7}}}`,
	)
}

// TestResponsesStream_EventSequence is the consumer-facing contract: the same
// message_start → content_block_start → deltas → content_block_stop →
// message_delta → message_stop shape every other provider emits, with dense
// block indices that do not collide between text and tool calls.
func TestResponsesStream_EventSequence(t *testing.T) {
	var gotReq respRequest
	srv := responsesStreamServer(t, &gotReq, happyResponsesStream())
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-5.6-sol", 1024)
	var events []StreamEvent
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "ls please"}},
		chatTestTools, "sys",
		func(ev StreamEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// The request opts into streaming — and must NOT carry stream_options,
	// which belongs to the other dialect.
	if !gotReq.Stream {
		t.Error("request did not set stream=true")
	}

	want := []StreamEvent{
		{Type: StreamEventMessageStart},
		// The reasoning item allocates no block, so the text message is 0.
		{Type: StreamEventContentBlockStart, Index: 0},
		{Type: StreamEventTextDelta, Index: 0, Text: "Hel"},
		{Type: StreamEventTextDelta, Index: 0, Text: "lo"},
		{Type: StreamEventContentBlockStop, Index: 0},
		{Type: StreamEventContentBlockStart, Index: 1, ToolUseID: "call_a", ToolUseName: "bash"},
		{Type: StreamEventToolUseDelta, Index: 1, PartialJSON: `{"command":`},
		{Type: StreamEventToolUseDelta, Index: 1, PartialJSON: `"ls"}`},
		{Type: StreamEventContentBlockStop, Index: 1},
		{Type: StreamEventMessageDelta, StopReason: StopReasonToolUse, Usage: Usage{
			InputTokens: 42, OutputTokens: 7, CacheReadInputTokens: 5, CacheCreationInputTokens: 1,
		}},
		{Type: StreamEventMessageStop},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d:\n%+v", len(events), len(want), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, events[i], want[i])
		}
	}

	// The assembled response comes from the terminal event's own view.
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Hello" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_a" ||
		string(resp.ToolCalls[0].Input) != `{"command":"ls"}` {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if (resp.Usage != Usage{InputTokens: 42, OutputTokens: 7, CacheReadInputTokens: 5, CacheCreationInputTokens: 1}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestResponsesStream_NativeAndShimAgree: the native ChatProvider path and the
// compat-adapter path must stream identically — same request bytes, same
// events, same result. This is the streaming half of the differential bar.
func TestResponsesStream_NativeAndShimAgree(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "text/event-stream", happyResponsesStream())
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-5.6-sol", 1024)
	turns := TurnsFromConversation(responsesConversation())
	for variant, req := range chatRequestVariants(turns, chatTestTools) {
		t.Run(variant, func(t *testing.T) {
			diffChatStream(t, &bodies, mustBeNative(t, p), mustBeAdapter(t, fullSeamAgentic{p}), req)
		})
	}
}

// TestResponsesStream_Truncated: a dropped connection leaves no terminal event,
// so the parser must return what arrived plus an error — and a tool call cut
// mid-arguments must fall back to a valid empty object, or it poisons every
// later request at marshal time.
func TestResponsesStream_Truncated(t *testing.T) {
	body := fakeResponsesSSE(
		`{"type":"response.created","sequence_number":0}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_t","name":"browser_action","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"action\":\"navi"}`,
		// connection drops here: no terminal event
	)

	resp, err := parseResponsesStream(strings.NewReader(body), "openai", nil)
	if err == nil {
		t.Error("expected a truncated-stream error, got nil")
	}
	if resp == nil || len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "partial" {
		t.Fatalf("expected the partial text, got %+v", resp)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected the partial tool call, got %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_t" || tc.Name != "browser_action" {
		t.Errorf("tool call identity lost: %+v", tc)
	}
	if !json.Valid(tc.Input) || string(tc.Input) != "{}" {
		t.Errorf("truncated tool input = %q, want {}", tc.Input)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("stop = %q, want it inferred from the partial tool call", resp.StopReason)
	}
}

// TestResponsesStream_TerminalStates: the three ways a stream can end. Only
// completed is a success; incomplete on the cap must reach the orchestrator as
// StopReasonMaxTokens, and failed must surface the server's reason rather than
// looking like a clean short answer.
func TestResponsesStream_TerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
		wantErr  string
		wantStop StopReason
	}{
		{
			name:     "completed",
			terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			wantStop: StopReasonEndTurn,
		},
		{
			name:     "incomplete on the output cap",
			terminal: `{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":1}}}`,
			wantStop: StopReasonMaxTokens,
		},
		{
			name:     "failed",
			terminal: `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"message":"server had an oops"}}}`,
			wantErr:  "server had an oops",
			wantStop: StopReasonEndTurn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fakeResponsesSSE(
				`{"type":"response.created","sequence_number":0}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
				`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
				tc.terminal,
			)
			var sawStop bool
			resp, err := parseResponsesStream(strings.NewReader(body), "openai",
				func(ev StreamEvent) {
					if ev.Type == StreamEventMessageStop {
						sawStop = true
					}
				})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
			if resp.StopReason != tc.wantStop {
				t.Errorf("stop = %q, want %q", resp.StopReason, tc.wantStop)
			}
			// Even a failure closes the message: consumers that only tear down
			// their spinner on message_stop would otherwise hang.
			if !sawStop {
				t.Error("no message_stop event was emitted")
			}
		})
	}
}

// TestResponsesStream_MidStreamError: an in-band error event aborts with the
// server's message.
func TestResponsesStream_MidStreamError(t *testing.T) {
	body := fakeResponsesSSE(
		`{"type":"response.created","sequence_number":0}`,
		`{"type":"error","error":{"message":"rate limited mid-stream"}}`,
	)
	if _, err := parseResponsesStream(strings.NewReader(body), "openai", nil); err == nil ||
		!strings.Contains(err.Error(), "rate limited mid-stream") {
		t.Errorf("err = %v, want the mid-stream error message", err)
	}
}

// TestResponsesStream_SkipsUnparseableFrames: one unreadable frame must not
// abort an otherwise good turn.
func TestResponsesStream_SkipsUnparseableFrames(t *testing.T) {
	body := fakeResponsesSSE(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"a"}`,
	) + "data: {not json at all\n\n" + fakeResponsesSSE(
		`{"type":"response.output_text.delta","output_index":0,"delta":"b"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ab"}]}],"usage":{}}}`,
	)

	resp, err := parseResponsesStream(strings.NewReader(body), "openai", nil)
	if err != nil {
		t.Fatalf("one bad frame aborted the stream: %v", err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "ab" {
		t.Errorf("text blocks = %+v, want %q", resp.TextBlocks, "ab")
	}
}

// TestResponsesStream_BadRequestDegradesToNonStreaming: a server that refuses
// to stream still completes the turn. There is no stream_options stage to
// retry here (this dialect has no such field), so the ladder is exactly two
// requests — the streaming attempt and the non-streaming one.
func TestResponsesStream_BadRequestDegradesToNonStreaming(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var req respRequest
		_ = json.Unmarshal(raw, &req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"streaming is not available here"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"fallback"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-5.6-sol", 0)
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err != nil {
		t.Fatalf("expected degrade to non-streaming, got %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one stream attempt + the non-streaming retry)", calls)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "fallback" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestResponsesStream_AuthErrorNotDegraded: a 401 fails identically unstreamed,
// so it must surface immediately rather than burning another request.
func TestResponsesStream_AuthErrorNotDegraded(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "bad", srv.URL, "gpt-5.6-sol", 1024)
	if _, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil); err == nil {
		t.Fatal("a 401 must surface, not degrade")
	} else if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want it to name the status", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on auth failure)", calls)
	}
}
