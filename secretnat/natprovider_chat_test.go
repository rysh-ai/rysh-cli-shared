package secretnat

// ChatProvider-path sanitization proofs (design 002 A1 final step).
//
// AsChatProvider fast-paths the BARE native providers to their native
// Chat/ChatStream. A secretnat-wrapped provider must NEVER take that fast
// path: the wrapper's sanitization lives in CompleteWithTools /
// CompleteWithToolsStream, so bypassing the compat adapter would send raw
// secrets to the wire. These tests drive a real Claude provider over httptest
// through provider.AsChatProvider(...).Chat/ChatStream and assert the raw
// wire bytes carry no secrets — for the real NATProvider wrapper AND for the
// embedding shape the fast-path guard exists to defeat.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

const chatClaudeJSONBody = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
	`"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

// chatCaptureServer records raw request bodies and answers with body.
func chatCaptureServer(bodies *[][]byte, contentType, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, raw)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
}

func secretChatRequest() provider.ChatRequest {
	return provider.ChatRequest{
		System: "system uses " + stripeKey,
		Turns:  provider.TurnsFromConversation(secretConversation()),
	}
}

func assertWireClean(t *testing.T, body []byte) {
	t.Helper()
	s := string(body)
	if strings.Contains(s, stripeKey) {
		t.Fatalf("stripe key leaked to the wire: %s", s)
	}
	if strings.Contains(s, githubKey) {
		t.Fatalf("github key leaked to the wire: %s", s)
	}
}

// TestNATWrappedProviderChatSanitizes: a secretnat-wrapped native provider
// must go through the compat adapter (NOT the native fast-path), so every
// Chat/ChatStream request is sanitized before it reaches the wire.
func TestNATWrappedProviderChatSanitizes(t *testing.T) {
	var bodies [][]byte
	srv := chatCaptureServer(&bodies, "application/json", chatClaudeJSONBody)
	defer srv.Close()

	inner := provider.NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	w := Wrap(inner, natSession(t))

	cp := provider.AsChatProvider(w)
	if any(cp) == any(w) || any(cp) == any(provider.AgenticProvider(inner)) {
		t.Fatalf("wrapped provider must be adapted, got %T", cp)
	}

	if _, err := cp.Chat(context.Background(), secretChatRequest()); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(bodies))
	}
	assertWireClean(t, bodies[0])

	// The streaming capability must survive the wrap+adapt, and the streaming
	// path must sanitize identically.
	var streamBodies [][]byte
	streamSrv := chatCaptureServer(&streamBodies, "text/event-stream",
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	defer streamSrv.Close()
	streamInner := provider.NewClaudeAgenticProvider("k", streamSrv.URL, "claude-x", 0)
	scp := provider.AsChatProvider(Wrap(streamInner, natSession(t)))
	if !provider.ChatStreamSupported(scp) {
		t.Fatal("adapter over the wrapped streaming provider must report streaming")
	}
	if _, err := scp.ChatStream(context.Background(), secretChatRequest(), nil); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if len(streamBodies) != 1 {
		t.Fatalf("expected 1 stream request, got %d", len(streamBodies))
	}
	assertWireClean(t, streamBodies[0])
}

// embeddedNATClaude is the trap the fast-path guard exists for: a sanitizing
// wrapper that EMBEDS the concrete provider. Method promotion makes it
// satisfy ChatProvider (and the native marker) with the EMBEDDED provider's
// methods — a naive AsChatProvider fast-path would call those directly and
// silently skip sanitization. The guard's identity check must keep it on the
// adapter path, where the CompleteWithTools override below applies.
type embeddedNATClaude struct {
	*provider.ClaudeAgenticProvider
	nat NATProvider // reuses the wrapper's sanitizeRequest
}

func (e *embeddedNATClaude) CompleteWithTools(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
) (*provider.AgenticResponse, error) {
	return e.nat.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// TestEmbeddedNATProviderChatSanitizes fails if the AsChatProvider wrapper
// guard is removed (i.e. if any ChatProvider implementer were fast-pathed):
// the promoted native Chat would send the raw secrets.
func TestEmbeddedNATProviderChatSanitizes(t *testing.T) {
	var bodies [][]byte
	srv := chatCaptureServer(&bodies, "application/json", chatClaudeJSONBody)
	defer srv.Close()

	inner := provider.NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	e := &embeddedNATClaude{
		ClaudeAgenticProvider: inner,
		nat:                   NATProvider{inner: inner, nat: natSession(t)},
	}
	// The embedding satisfies ChatProvider structurally — exactly the shape
	// that must NOT be fast-pathed.
	if _, ok := provider.AgenticProvider(e).(provider.ChatProvider); !ok {
		t.Fatal("test premise broken: embedding no longer satisfies ChatProvider")
	}

	cp := provider.AsChatProvider(e)
	if any(cp) == any(provider.AgenticProvider(e)) || any(cp) == any(provider.AgenticProvider(inner)) {
		t.Fatalf("embedding wrapper must be adapted, got %T", cp)
	}

	if _, err := cp.Chat(context.Background(), secretChatRequest()); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(bodies))
	}
	assertWireClean(t, bodies[0])
}
