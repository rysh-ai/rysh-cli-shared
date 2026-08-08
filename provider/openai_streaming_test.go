package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOAISSE builds an OpenAI-compatible SSE response body: one `data:` line
// per payload, terminated by the caller passing "[DONE]" explicitly so tests
// can also exercise truncated streams.
func fakeOAISSE(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: ")
		sb.WriteString(p)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// The provider family in these tests is deliberately ollama/gemini rather than
// openai: this file covers the CHAT COMPLETIONS streaming dialect, which is
// what those endpoints speak. OpenAI proper streams the Responses API — see
// openai_responses_streaming_test.go.

// sseServer serves body for POST /chat/completions and captures the decoded
// request into gotReq.
func sseServer(t *testing.T, gotReq *oaiRequest, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, gotReq)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
}

// TestOpenAIStream_TextAssemblesAndUsage covers the happy path: content
// streamed across chunks assembles into one text block, deltas reach the
// callback in order, and the trailing usage chunk (sent because the request
// set stream_options.include_usage) lands in resp.Usage.
func TestOpenAIStream_TextAssemblesAndUsage(t *testing.T) {
	var gotReq oaiRequest
	srv := sseServer(t, &gotReq, fakeOAISSE(
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo, "},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7}}`,
		`[DONE]`,
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("gemini", "k", srv.URL, "gemini-2.5-flash", 1024)
	var deltas []string
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "sys",
		func(ev StreamEvent) {
			if ev.Type == StreamEventTextDelta {
				deltas = append(deltas, ev.Text)
			}
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// The request must opt in to streaming + usage.
	if !gotReq.Stream {
		t.Errorf("request did not set stream=true")
	}
	if gotReq.StreamOptions == nil || !gotReq.StreamOptions.IncludeUsage {
		t.Errorf("request did not set stream_options.include_usage: %+v", gotReq.StreamOptions)
	}

	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Hello, world" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got := strings.Join(deltas, "|"); got != "Hel|lo, |world" {
		t.Errorf("deltas = %q", got)
	}
}

// TestOpenAIStream_ToolCallDeltasAssemble exercises tool_call fragments:
// id/name arrive on the first fragment, arguments accumulate across chunks,
// and a second parallel tool call (its own tool index) assembles
// independently. finish_reason=tool_calls maps to StopReasonToolUse.
func TestOpenAIStream_ToolCallDeltasAssemble(t *testing.T) {
	var gotReq oaiRequest
	srv := sseServer(t, &gotReq, fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"file_read","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"grep","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3.1", 0)
	var starts []string
	var partials []string
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "read a.go"}},
		[]ToolSpec{{Name: "file_read", Parameters: json.RawMessage(`{"type":"object"}`)}}, "",
		func(ev StreamEvent) {
			switch ev.Type {
			case StreamEventContentBlockStart:
				starts = append(starts, ev.ToolUseName)
			case StreamEventToolUseDelta:
				partials = append(partials, ev.PartialJSON)
			}
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	a, b := resp.ToolCalls[0], resp.ToolCalls[1]
	if a.ID != "call_a" || a.Name != "file_read" || string(a.Input) != `{"path":"a.go"}` {
		t.Errorf("tool a = %+v (input %s)", a, a.Input)
	}
	if b.ID != "call_b" || b.Name != "grep" || string(b.Input) != `{"q":"x"}` {
		t.Errorf("tool b = %+v (input %s)", b, b.Input)
	}
	if !json.Valid(a.Input) || !json.Valid(b.Input) {
		t.Errorf("tool inputs not valid JSON: %s / %s", a.Input, b.Input)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if got := strings.Join(starts, "|"); got != "file_read|grep" {
		t.Errorf("block starts = %q", got)
	}
	if got := strings.Join(partials, ""); got != `{"path":"a.go"}{"q":"x"}` {
		t.Errorf("partial json = %q", got)
	}
}

// TestOpenAIStream_MissingUsageTolerated covers servers (older Ollama) that
// ignore stream_options and never send a usage chunk: the stream still
// assembles cleanly with zero-valued usage and no error.
func TestOpenAIStream_MissingUsageTolerated(t *testing.T) {
	var gotReq oaiRequest
	srv := sseServer(t, &gotReq, fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3.1", 0)
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "ok" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if resp.Usage != (Usage{}) {
		t.Errorf("usage should be zero, got %+v", resp.Usage)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

// TestOpenAIStream_BadRequestDegradesToNonStreaming covers the setup-failure
// fallback: a server that 400s the streaming request (e.g. it rejects
// stream_options) gets one non-streaming retry, whose response is returned.
func TestOpenAIStream_BadRequestDegradesToNonStreaming(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var req oaiRequest
		_ = json.Unmarshal(raw, &req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"stream_options is not supported"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "k", srv.URL, "llama3.1", 0)
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err != nil {
		t.Fatalf("expected degrade to non-streaming, got %v", err)
	}
	// This server rejects streaming outright, so all three degrade stages run:
	// stream+stream_options, stream alone (the stage that keeps older Ollama
	// streaming — see TestOpenAIStream_RetriesWithoutStreamOptions), then the
	// non-streaming call.
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (two stream attempts + non-streaming retry)", calls)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "fallback" {
		t.Errorf("text blocks = %+v", resp.TextBlocks)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestParseOpenAIStream_TruncatedStream simulates a connection drop before
// [DONE]: the parser returns the partial text plus a non-nil error, and a
// tool call whose arguments were cut mid-JSON falls back to a valid empty
// object so it can never poison later request marshalling (same guard as
// parseClaudeStream).
func TestParseOpenAIStream_TruncatedStream(t *testing.T) {
	body := fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_t","function":{"name":"browser_action","arguments":"{\"action\":\"navi"}}]},"finish_reason":null}]}`,
		// connection drops here: no finish_reason, no [DONE]
	)
	resp, err := parseOpenAIStream(strings.NewReader(body), "openai", nil)
	if err == nil {
		t.Errorf("expected truncated-stream error, got nil")
	}
	if resp == nil || len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "partial" {
		t.Fatalf("expected partial text, got %+v", resp)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 partial tool call, got %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if !json.Valid(tc.Input) || string(tc.Input) != "{}" {
		t.Errorf("truncated tool input = %q, want {}", string(tc.Input))
	}
	if _, mErr := json.Marshal(tc.Input); mErr != nil {
		t.Errorf("re-marshal of tool input failed: %v", mErr)
	}
}

// TestOpenAIStream_MidStreamErrorChunk: an in-band error object aborts the
// stream with the server's message.
func TestOpenAIStream_MidStreamErrorChunk(t *testing.T) {
	body := fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		`{"error":{"message":"rate limited mid-stream"}}`,
	)
	_, err := parseOpenAIStream(strings.NewReader(body), "openai", nil)
	if err == nil || !strings.Contains(err.Error(), "rate limited mid-stream") {
		t.Errorf("err = %v, want mid-stream error message", err)
	}
}
