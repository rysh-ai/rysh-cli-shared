package provider

// Tests for the per-request MaxTokens seam (design 002 follow-up).
// ChatRequest.MaxTokens must reach the wire request's max_tokens field for
// both concrete providers, streaming and non-streaming; 0 keeps the
// construction-time default; providers without the seam keep erroring loudly
// (TestChatRequest_ModelOverride). The wrapper pass-through contract is
// covered in secretnat/natprovider_test.go.

import (
	"context"
	"encoding/json"
	"testing"
)

// wireMaxTokens decodes the max_tokens field of a captured request body.
func wireMaxTokens(t *testing.T, body []byte) int {
	t.Helper()
	var sent struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	return sent.MaxTokens
}

var maxTokensTestTurns = []Turn{{Role: RoleUser, Blocks: []Block{{Kind: BlockKindText, Text: "hi"}}}}

// seamForwardingAgentic hides ChatProvider (forcing AsChatProvider onto the
// compat adapter) while forwarding the MaxTokens seam — the shape of the
// secretnat / rysh-cli decorators that pass the override through.
type seamForwardingAgentic struct{ AgenticProvider }

func (s seamForwardingAgentic) WithMaxTokens(maxTokens int) AgenticProvider {
	return s.AgenticProvider.(MaxTokensOverridable).WithMaxTokens(maxTokens)
}

// TestWithMaxTokens covers the seam wrapper itself on both concrete
// providers: a positive cap replaces, <= 0 keeps, the receiver is never
// mutated. Mirrors TestWithModelEffort.
func TestWithMaxTokens(t *testing.T) {
	base := NewClaudeAgenticProvider("k", "", "claude-opus-4-8", 1024)
	over := base.WithMaxTokens(64).(*ClaudeAgenticProvider)
	if over.maxTokens != 64 {
		t.Errorf("override not applied: maxTokens=%d", over.maxTokens)
	}
	if base.maxTokens != 1024 {
		t.Errorf("receiver mutated: maxTokens=%d", base.maxTokens)
	}
	if kept := over.WithMaxTokens(0).(*ClaudeAgenticProvider); kept.maxTokens != 64 {
		t.Errorf("zero override should keep the cap, got %d", kept.maxTokens)
	}

	oai := NewOpenAIAgenticProvider("openai", "k", "", "gpt-x", 512)
	oover := oai.WithMaxTokens(64).(*OpenAIAgenticProvider)
	if oover.maxTokens != 64 || oai.maxTokens != 512 {
		t.Errorf("openai override wrong: over=%d base=%d", oover.maxTokens, oai.maxTokens)
	}
	if kept := oover.WithMaxTokens(-1).(*OpenAIAgenticProvider); kept.maxTokens != 64 {
		t.Errorf("negative override should keep the cap, got %d", kept.maxTokens)
	}
}

// TestChatRequest_MaxTokens_Claude: the override lands in the non-streaming
// Claude wire body (direct impl AND compat adapter), 0 keeps the per-model
// default, Model+MaxTokens compose, and a negative value errors loudly.
func TestChatRequest_MaxTokens_Claude(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", claudeJSONBody)
	defer srv.Close()

	p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	ctx := context.Background()

	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 777}); err != nil {
		t.Fatalf("chat with MaxTokens: %v", err)
	}
	if got := wireMaxTokens(t, bodies[0]); got != 777 {
		t.Errorf("wire max_tokens = %d, want 777", got)
	}

	// 0 keeps the construction-time default.
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns}); err != nil {
		t.Fatalf("chat without MaxTokens: %v", err)
	}
	if got, want := wireMaxTokens(t, bodies[1]), DefaultMaxTokensForModel("claude-x"); got != want {
		t.Errorf("default wire max_tokens = %d, want %d", got, want)
	}

	// The compat adapter over a seam-forwarding wrapper routes through the
	// same seam; a wrapper that HIDES the seam (opaqueAgentic) must error
	// loudly instead.
	if _, err := AsChatProvider(seamForwardingAgentic{p}).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 888}); err != nil {
		t.Fatalf("adapter chat with MaxTokens: %v", err)
	}
	if got := wireMaxTokens(t, bodies[2]); got != 888 {
		t.Errorf("adapter wire max_tokens = %d, want 888", got)
	}

	// Model + MaxTokens overrides compose.
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, Model: "claude-y", MaxTokens: 999}); err != nil {
		t.Fatalf("composed overrides: %v", err)
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(bodies[3], &sent); err != nil || sent.Model != "claude-y" || sent.MaxTokens != 999 {
		t.Errorf("composed request = %+v (err %v), want model claude-y max_tokens 999", sent, err)
	}

	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: -5}); err == nil {
		t.Error("negative MaxTokens must error loudly")
	}
	if _, err := AsChatProvider(opaqueAgentic{p}).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 5}); err == nil {
		t.Error("a wrapper hiding the seam must error loudly, not silently drop the cap")
	}
}

// TestChatRequest_MaxTokens_ClaudeStream: same wire proof on the streaming
// path (stream=true request body).
func TestChatRequest_MaxTokens_ClaudeStream(t *testing.T) {
	sse := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	var bodies [][]byte
	srv := captureServer(&bodies, "text/event-stream", sse)
	defer srv.Close()

	p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	ctx := context.Background()

	if _, err := AsChatProvider(p).ChatStream(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 321}, nil); err != nil {
		t.Fatalf("chat stream with MaxTokens: %v", err)
	}
	var sent struct {
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil || sent.MaxTokens != 321 || !sent.Stream {
		t.Errorf("stream request = %+v (err %v), want max_tokens 321 stream true", sent, err)
	}

	if _, err := AsChatProvider(p).ChatStream(ctx, ChatRequest{Turns: maxTokensTestTurns}, nil); err != nil {
		t.Fatalf("chat stream without MaxTokens: %v", err)
	}
	if got, want := wireMaxTokens(t, bodies[1]), DefaultMaxTokensForModel("claude-x"); got != want {
		t.Errorf("default stream max_tokens = %d, want %d", got, want)
	}
}

// TestChatRequest_MaxTokens_OpenAI: same wire proofs for the
// OpenAI-compatible provider, non-streaming and streaming.
func TestChatRequest_MaxTokens_OpenAI(t *testing.T) {
	ctx := context.Background()

	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", oaiJSONBody)
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-x", 512)
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 777}); err != nil {
		t.Fatalf("chat with MaxTokens: %v", err)
	}
	if got := wireMaxTokens(t, bodies[0]); got != 777 {
		t.Errorf("wire max_tokens = %d, want 777", got)
	}
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns}); err != nil {
		t.Fatalf("chat without MaxTokens: %v", err)
	}
	if got := wireMaxTokens(t, bodies[1]); got != 512 {
		t.Errorf("default wire max_tokens = %d, want 512", got)
	}

	sse := fakeOAISSE(
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	var streamBodies [][]byte
	streamSrv := captureServer(&streamBodies, "text/event-stream", sse)
	defer streamSrv.Close()

	sp := NewOpenAIAgenticProvider("ollama", "", streamSrv.URL, "llama3", 256)
	if _, err := AsChatProvider(sp).ChatStream(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 321}, nil); err != nil {
		t.Fatalf("chat stream with MaxTokens: %v", err)
	}
	var sent struct {
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}
	if err := json.Unmarshal(streamBodies[0], &sent); err != nil || sent.MaxTokens != 321 || !sent.Stream {
		t.Errorf("stream request = %+v (err %v), want max_tokens 321 stream true", sent, err)
	}
	if _, err := AsChatProvider(sp).ChatStream(ctx, ChatRequest{Turns: maxTokensTestTurns}, nil); err != nil {
		t.Fatalf("chat stream without MaxTokens: %v", err)
	}
	if got := wireMaxTokens(t, streamBodies[1]); got != 256 {
		t.Errorf("default stream max_tokens = %d, want 256", got)
	}
}
