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

// Coverage for the streaming degrade ladder and the structural contract that
// opts this provider in. Complements openai_streaming_test.go, which covers the
// parser; these tests are about what happens when a server pushes back.

// TestOpenAIStream_ImplementsStreamingProvider is the wiring guarantee. Nothing
// references CompleteWithToolsStream by name — the orchestrator and every
// wrapper opt in by type assertion — so losing the method would silently
// downgrade openai/ollama/gemini to blocking calls with no test failing.
func TestOpenAIStream_ImplementsStreamingProvider(t *testing.T) {
	var p AgenticProvider = NewOpenAIAgenticProvider("openai", "k", "http://x/v1", "gpt-4o", 64)
	if _, ok := p.(StreamingProvider); !ok {
		t.Fatal("OpenAIAgenticProvider does not satisfy StreamingProvider")
	}
}

// TestOpenAIStream_RetriesWithoutStreamOptions is the first degrade stage: a
// server that rejects stream_options but streams fine must keep streaming.
// Falling straight through to the non-streaming path would silently cost
// air-gapped Ollama users their incremental output.
func TestOpenAIStream_RetriesWithoutStreamOptions(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		bodies = append(bodies, decoded)
		if _, hasSO := decoded["stream_options"]; hasSO {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown field stream_options"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fakeOAISSE(
			`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			"[DONE]",
		))
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3", 1024)
	var deltas int
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "",
		func(ev StreamEvent) {
			if ev.Type == StreamEventTextDelta {
				deltas++
			}
		})
	if err != nil {
		t.Fatalf("stream_options rejection must degrade, got %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (with, then without stream_options)", len(bodies))
	}
	if _, hasSO := bodies[1]["stream_options"]; hasSO {
		t.Error("retry still sent stream_options")
	}
	// The point of the retry: streaming is preserved, not abandoned.
	if bodies[1]["stream"] != true {
		t.Error("retry dropped stream:true; it must only drop stream_options")
	}
	if deltas == 0 {
		t.Error("retry produced no text deltas — streaming was lost")
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "ok" {
		t.Errorf("TextBlocks = %+v", resp.TextBlocks)
	}
}

// TestOpenAIStream_AuthErrorNotDegraded: a 401 fails identically unstreamed, so
// it must surface immediately rather than burning two more requests.
func TestOpenAIStream_AuthErrorNotDegraded(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "bad", srv.URL, "gpt-4o", 1024)
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

// TestOpenAIStream_BadPayloadSurfacesError: when the payload really is bad,
// every stage fails and the caller must get the server's reason, not silence.
func TestOpenAIStream_BadPayloadSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "nope", 1024)
	_, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err == nil {
		t.Fatal("a genuinely bad payload must surface an error")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("err = %v, want the server's reason", err)
	}
}

// TestOpenAIStream_NoFinishReasonInfersStop: Ollama frequently omits
// finish_reason. Reporting the zero StopReason would skip the orchestrator's
// tool-use branch and strand the tool call.
func TestOpenAIStream_NoFinishReasonInfersStop(t *testing.T) {
	srv := sseServer(t, nil, fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{}"}}]}}]}`,
		"[DONE]",
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3", 1024)
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "go"}}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %q, want %q inferred from the tool call", resp.StopReason, StopReasonToolUse)
	}
}

// TestOpenAIStream_TextAndToolIndicesDoNotCollide: OpenAI numbers tool calls
// from 0 and gives text no index at all, so a consumer keying events by Index
// would merge the text block with tool 0 unless the namespaces are separated.
func TestOpenAIStream_TextAndToolIndicesDoNotCollide(t *testing.T) {
	srv := sseServer(t, nil, fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		"[DONE]",
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "k", srv.URL, "llama3.1", 1024)
	textIdx, toolIdx := -1, -2
	_, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "go"}}, nil, "",
		func(ev StreamEvent) {
			switch ev.Type {
			case StreamEventTextDelta:
				textIdx = ev.Index
			case StreamEventToolUseDelta:
				toolIdx = ev.Index
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if textIdx == toolIdx {
		t.Errorf("text and tool deltas share Index %d; the namespaces must not collide", textIdx)
	}
}

// TestOpenAIStream_SkipsUnparseableFrames: the dialect has many server
// implementations, so one unreadable frame must not abort an otherwise good
// turn.
func TestOpenAIStream_SkipsUnparseableFrames(t *testing.T) {
	srv := sseServer(t, nil, fakeOAISSE(
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{not json at all`,
		`{"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}`,
		"[DONE]",
	))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "k", srv.URL, "llama3.1", 1024)
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "ab" {
		t.Errorf("TextBlocks = %+v, want %q", resp.TextBlocks, "ab")
	}
}
