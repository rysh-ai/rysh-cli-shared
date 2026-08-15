// SPDX-License-Identifier: Apache-2.0

package provider

// The reported failure, as a test: a transient transport error on the OpenAI
// streaming request killed the turn, because this provider had no retry policy
// while the Claude one did.
//
//	[LLM error: openai: stream request: Post ".../v1/responses": EOF]

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry is the production policy with the sleeps removed, so a test that
// exercises four attempts does not spend 15 seconds backing off.
func fastRetry() RetryPolicy {
	p := DefaultRetryPolicy()
	p.BaseDelay = time.Millisecond
	p.MaxDelay = 2 * time.Millisecond
	p.Jitter = 0
	return p
}

// respTextOpen opens a streamed assistant message. The Responses parser needs
// the item lifecycle before a delta becomes a client-visible text event, so a
// bare delta would reach no callback at all.
const respTextOpen = `{"type":"response.output_item.added","output_index":0,` +
	`"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`

// respTextDelta streams one chunk of that message.
func respTextDelta(text string) string {
	return `{"type":"response.output_text.delta","output_index":0,"content_index":0,` +
		`"item_id":"msg_1","delta":"` + text + `"}`
}

// respCompleted terminates the stream with the assembled text.
func respCompleted(text string) string {
	return `{"type":"response.completed","response":{"status":"completed","output":` +
		`[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"` + text + `"}]}],"usage":{}}}`
}

// respOKBody is a minimal successful Responses reply.
const respOKBody = `{"status":"completed","output":[{"type":"message","content":` +
	`[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`

func retryProvider(t *testing.T, url string) *OpenAIAgenticProvider {
	t.Helper()
	p := NewOpenAIAgenticProvider("openai", "k", url, "gpt-5.6-luna", 256)
	p.SetRetryPolicy(fastRetry())
	return p
}

// hijackEOF closes the connection without writing a response, reproducing the
// stale-keep-alive race the user hit: the client's Do returns a bare EOF with
// no HTTP reply to classify.
func hijackEOF(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestOpenAIStreamRetriesTransportEOF is the reported bug. The first attempt
// dies exactly as the user's did; the turn must still complete.
func TestOpenAIStreamRetriesTransportEOF(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			hijackEOF(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fakeResponsesSSE(
			respTextOpen, respTextDelta("recovered"), respCompleted("recovered")))
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	var got strings.Builder
	resp, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "",
		func(ev StreamEvent) {
			if ev.Type == StreamEventTextDelta {
				got.WriteString(ev.Text)
			}
		})
	if err != nil {
		t.Fatalf("a transient EOF still killed the turn: %v", err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("server saw %d attempts; the request was not retried", attempts.Load())
	}
	if got.String() != "recovered" {
		t.Errorf("streamed %q, want %q", got.String(), "recovered")
	}
	if resp == nil {
		t.Error("no response after a successful retry")
	}
}

// TestOpenAINonStreamingRetriesTransportEOF: the same gap existed on the
// one-shot path, which is also where the streaming degrade ladder ends up.
func TestOpenAINonStreamingRetriesTransportEOF(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			hijackEOF(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respOKBody)
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	if _, err := p.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, ""); err != nil {
		t.Fatalf("transient EOF killed the non-streaming turn: %v", err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("server saw %d attempts; the request was not retried", attempts.Load())
	}
}

// TestOpenAIRetriesRateLimitAndHonoursRetryAfter: 429 is the other failure the
// missing policy dropped on the floor, and the server's Retry-After is the
// wait it asked for.
func TestOpenAIRetriesRateLimitAndHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respOKBody)
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	if _, err := p.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, ""); err != nil {
		t.Fatalf("429 was not retried: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("server saw %d attempts, want 2", attempts.Load())
	}
}

// TestOpenAIDoesNotRetryFatalStatuses: a bad key is not transient. Retrying it
// four more times just delays the error the user needs to see.
func TestOpenAIDoesNotRetryFatalStatuses(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	_, err := p.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "")
	if err == nil {
		t.Fatal("a 401 should fail")
	}
	if attempts.Load() != 1 {
		t.Errorf("server saw %d attempts for a 401, want 1", attempts.Load())
	}
	// The historic message shape must survive the move to a typed error.
	if !strings.Contains(err.Error(), "openai: status 401:") {
		t.Errorf("error text changed shape: %v", err)
	}
}

// TestOpenAIDoesNotReplayStreamedText is the guard that separates "fix a
// dropped connection" from "show the user half an answer twice". A stream that
// fails AFTER emitting deltas must surface the error, not start over.
func TestOpenAIDoesNotReplayStreamedText(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// Emit one delta, then cut the connection mid-stream.
		_, _ = io.WriteString(w, fakeResponsesSSE(respTextOpen, respTextDelta("partial")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hijackEOF(w)
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	var got strings.Builder
	_, _ = p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "",
		func(ev StreamEvent) {
			if ev.Type == StreamEventTextDelta {
				got.WriteString(ev.Text)
			}
		})
	if strings.Count(got.String(), "partial") > 1 {
		t.Errorf("streamed text was replayed to the user: %q", got.String())
	}
	if attempts.Load() != 1 {
		t.Errorf("retried after committing text to the screen: %d attempts", attempts.Load())
	}
}

// TestOpenAIRetryBudgetIsNotNested: the streaming degrade ladder ends in the
// non-streaming call. If that call carried its own retry budget the totals
// would multiply, turning one dropped request into 25.
func TestOpenAIRetryBudgetIsNotNested(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		hijackEOF(w)
	}))
	defer srv.Close()

	p := retryProvider(t, srv.URL)
	_, err := p.CompleteWithToolsStream(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "", nil)
	if err == nil {
		t.Fatal("expected failure once the budget is exhausted")
	}
	if max := int64(fastRetry().MaxAttempts); attempts.Load() > max {
		t.Errorf("server saw %d attempts, more than the %d-attempt budget — budgets are nesting", attempts.Load(), max)
	}
}

// TestOpenAIRetryPolicyIsOnByDefault: the fix is worthless if a provider built
// the normal way still has no policy.
func TestOpenAIRetryPolicyIsOnByDefault(t *testing.T) {
	p := NewOpenAIAgenticProvider("openai", "k", "http://x/v1", "gpt-4o", 64)
	if p.retryPolicy.MaxAttempts < 2 {
		t.Errorf("MaxAttempts = %d; a fresh provider does not retry", p.retryPolicy.MaxAttempts)
	}
	// And the per-request override seams must carry it, or `##pane model` would
	// silently produce a provider with retries off.
	if cp, ok := p.WithMaxTokens(128).(*OpenAIAgenticProvider); !ok || cp.retryPolicy.MaxAttempts != p.retryPolicy.MaxAttempts {
		t.Error("WithMaxTokens dropped the retry policy")
	}
	if cp, ok := p.WithModelEffort("gpt-4o-mini", "").(*OpenAIAgenticProvider); !ok || cp.retryPolicy.MaxAttempts != p.retryPolicy.MaxAttempts {
		t.Error("WithModelEffort dropped the retry policy")
	}
}

// TestClassifyOpenAIError pins the mapping the policy runs on.
func TestClassifyOpenAIError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want retryClassification
	}{
		{"nil", nil, classSuccess},
		{"429", &openaiHTTPError{status: 429}, classTransient},
		{"500", &openaiHTTPError{status: 500}, classTransient},
		{"529", &openaiHTTPError{status: 529}, classTransient},
		{"401", &openaiHTTPError{status: 401}, classFatal},
		{"400", &openaiHTTPError{status: 400}, classFatal},
		{"transport EOF", io.EOF, classTransient},
		{"cancelled", context.Canceled, classFatal},
	}
	for _, c := range cases {
		if got, _ := classifyOpenAIError(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// Retry-After is carried through for the policy to honour.
	if _, after := classifyOpenAIError(&openaiHTTPError{status: 429, retryAfter: 3 * time.Second}); after != 3*time.Second {
		t.Errorf("Retry-After not surfaced: %v", after)
	}
}
