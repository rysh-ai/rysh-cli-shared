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

// wireOutputCap decodes ALL THREE output-cap spellings from a captured body.
// OpenAI proper speaks the Responses API and sends max_output_tokens; Ollama
// and the Gemini compat layer speak Chat Completions and send max_tokens; the
// middle spelling, max_completion_tokens, is Chat Completions' newer field and
// must now never go out at all (OpenAI was its only user and has moved off that
// endpoint). Returning all three lets a test assert that exactly one went out —
// sending the wrong one is a 400 on every endpoint here.
func wireOutputCap(t *testing.T, body []byte) (maxTokens, maxCompletionTokens, maxOutputTokens int) {
	t.Helper()
	var sent struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
		MaxOutputTokens     int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	return sent.MaxTokens, sent.MaxCompletionTokens, sent.MaxOutputTokens
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

	// OpenAI proper: the cap must ride the Responses API's max_output_tokens,
	// with both Chat Completions spellings ABSENT — that endpoint accepts
	// neither.
	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-x", 512)
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns, MaxTokens: 777}); err != nil {
		t.Fatalf("chat with MaxTokens: %v", err)
	}
	if mt, mct, mot := wireOutputCap(t, bodies[0]); mt != 0 || mct != 0 || mot != 777 {
		t.Errorf("openai wire = max_tokens %d / max_completion_tokens %d / max_output_tokens %d, want 0 / 0 / 777", mt, mct, mot)
	}
	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns}); err != nil {
		t.Fatalf("chat without MaxTokens: %v", err)
	}
	if mt, mct, mot := wireOutputCap(t, bodies[1]); mt != 0 || mct != 0 || mot != 512 {
		t.Errorf("openai default wire = max_tokens %d / max_completion_tokens %d / max_output_tokens %d, want 0 / 0 / 512", mt, mct, mot)
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

// TestOutputCapFieldPerFamily pins the per-family wire split, endpoint and cap
// field together. It began as a two-way split when current OpenAI models
// started rejecting max_tokens outright (400 unsupported_parameter); those
// models then also refused function tools on Chat Completions, which moved
// OpenAI proper to the Responses API and its third spelling.
//
// OpenAI proper: POST /responses, max_output_tokens, neither Chat Completions
// spelling. Ollama and the Gemini compat layer: POST /chat/completions,
// max_tokens — they know neither newer field, and Ollama is what air-gapped
// mode runs on. Getting this wrong is a 400 on every endpoint involved.
func TestOutputCapFieldPerFamily(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		family              string
		wantPath            string
		wantMaxTokens       int
		wantCompletionField int
		wantOutputField     int
	}{
		{"openai", "/responses", 0, 0, 640},
		{"ollama", "/chat/completions", 640, 0, 0},
		{"gemini", "/chat/completions", 640, 0, 0},
		{"OpenAI", "/responses", 0, 0, 640}, // family match is case-insensitive
	} {
		t.Run(tc.family, func(t *testing.T) {
			var bodies [][]byte
			var paths []string
			srv := pathCaptureServer(&bodies, &paths, "application/json", oaiJSONBody)
			defer srv.Close()

			p := NewOpenAIAgenticProvider(tc.family, "k", srv.URL, "m", 640)
			if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: maxTokensTestTurns}); err != nil {
				t.Fatalf("chat: %v", err)
			}
			if paths[0] != tc.wantPath {
				t.Errorf("%s posted to %q, want %q", tc.family, paths[0], tc.wantPath)
			}
			mt, mct, mot := wireOutputCap(t, bodies[0])
			if mt != tc.wantMaxTokens || mct != tc.wantCompletionField || mot != tc.wantOutputField {
				t.Errorf("%s wire = max_tokens %d / max_completion_tokens %d / max_output_tokens %d, want %d / %d / %d",
					tc.family, mt, mct, mot, tc.wantMaxTokens, tc.wantCompletionField, tc.wantOutputField)
			}
		})
	}
}

// TestOutputCapOmittedWhenUnset: with no cap configured, neither field goes out
// — an explicit zero would be rejected by both dialects.
func TestOutputCapOmittedWhenUnset(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", oaiJSONBody)
	defer srv.Close()

	// maxTokens <= 0 is normalized to the 4096 default by the constructor, so
	// drive the zero case through the struct the constructor produces.
	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "m", 1)
	capped, ok := p.WithMaxTokens(0).(*OpenAIAgenticProvider)
	if !ok {
		t.Fatal("WithMaxTokens(0) did not return an *OpenAIAgenticProvider")
	}
	capped.maxTokens = 0
	if _, err := AsChatProvider(capped).Chat(context.Background(), ChatRequest{Turns: maxTokensTestTurns}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if mt, mct, mot := wireOutputCap(t, bodies[0]); mt != 0 || mct != 0 || mot != 0 {
		t.Errorf("unset cap wire = max_tokens %d / max_completion_tokens %d / max_output_tokens %d, want 0 / 0 / 0", mt, mct, mot)
	}
}
