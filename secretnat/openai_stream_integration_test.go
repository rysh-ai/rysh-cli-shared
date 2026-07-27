package secretnat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// The OpenAI-dialect streaming work is only useful if it survives the wrappers
// a real session puts between the orchestrator and the provider. Wrap()
// promotes to the streaming variant purely by type assertion, so a REAL
// OpenAIAgenticProvider (not a spy) must come out the other side still
// streaming — this is the check that "the code exists" and "a user gets
// streaming" are the same statement.

// oaiStreamServer serves a fixed Chat Completions SSE response.
func oaiStreamServer(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if seen != nil {
			*seen = string(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []string{
			`{"choices":[{"index":0,"delta":{"content":"hel"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
		}
	}))
}

// TestOpenAIProviderStreamsThroughNATWrap is the integration guarantee: a real
// OpenAI provider wrapped by SecretNAT still satisfies StreamingProvider, and
// the deltas reach the callback.
func TestOpenAIProviderStreamsThroughNATWrap(t *testing.T) {
	srv := oaiStreamServer(t, nil)
	defer srv.Close()

	inner := provider.NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-4o", 256)
	wrapped := Wrap(inner, natSession(t))

	streamer, ok := wrapped.(provider.StreamingProvider)
	if !ok {
		t.Fatal("a wrapped OpenAI provider no longer streams — Wrap must promote to natStreamingProvider")
	}

	var deltas []string
	resp, err := streamer.CompleteWithToolsStream(context.Background(),
		[]provider.ConversationTurn{{Role: "user", Content: "hi"}}, nil, "sys",
		func(ev provider.StreamEvent) {
			if ev.Type == provider.StreamEventTextDelta {
				deltas = append(deltas, ev.Text)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, ""); got != "hello" {
		t.Errorf("streamed text = %q, want %q", got, "hello")
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "hello" {
		t.Errorf("TextBlocks = %+v", resp.TextBlocks)
	}
}

// TestOpenAIStreamSanitizesOutbound proves the NAT layer still does its job on
// the streaming path: a secret in the conversation must not reach the wire.
// Streaming that bypassed sanitization would be a data leak, not a feature.
func TestOpenAIStreamSanitizesOutbound(t *testing.T) {
	var wire string
	srv := oaiStreamServer(t, &wire)
	defer srv.Close()

	inner := provider.NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-4o", 256)
	streamer, ok := Wrap(inner, natSession(t)).(provider.StreamingProvider)
	if !ok {
		t.Fatal("wrapped OpenAI provider does not stream")
	}
	_, err := streamer.CompleteWithToolsStream(context.Background(),
		secretConversation(), nil, "system uses "+stripeKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wire == "" {
		t.Fatal("no request body captured")
	}
	for _, secret := range []string{stripeKey, githubKey} {
		if strings.Contains(wire, secret) {
			t.Errorf("secret %q reached the wire on the streaming path:\n%s", secret, wire)
		}
	}
}
