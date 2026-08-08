package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// opaqueAgentic hides everything but the AgenticProvider interface, forcing
// AsChatProvider onto the compat-adapter path (the concrete providers
// implement ChatProvider directly and would be returned as-is).
type opaqueAgentic struct{ AgenticProvider }

// opaqueStreamingAgentic additionally passes through streaming, mirroring the
// shape of the rysh-cli / secretnat decorators.
type opaqueStreamingAgentic struct{ AgenticProvider }

func (o opaqueStreamingAgentic) CompleteWithToolsStream(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	cb StreamCallback,
) (*AgenticResponse, error) {
	return o.AgenticProvider.(StreamingProvider).CompleteWithToolsStream(ctx, conversation, tools, systemPrompt, cb)
}

// captureServer records every raw request body and answers each request with
// body/contentType.
func captureServer(bodies *[][]byte, contentType, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, raw)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
}

// pathCaptureServer is captureServer plus the request PATH, for the tests that
// pin which endpoint a provider family talks to (Chat Completions vs the
// Responses API) alongside what it sends.
func pathCaptureServer(bodies *[][]byte, paths *[]string, contentType, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, raw)
		*paths = append(*paths, r.URL.Path)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
}

var chatTestTools = []ToolSpec{
	{Name: "bash", Description: "run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
	{Name: "file_read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
}

const claudeJSONBody = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
	`"content":[{"type":"thinking","thinking":"pondering","signature":"sig-1"},` +
	`{"type":"text","text":"done"},` +
	`{"type":"tool_use","id":"call_9","name":"bash","input":{"command":"ls"}}],` +
	`"stop_reason":"tool_use",` +
	`"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`

// TestChatAdapter_Claude_RequestBytesIdentical proves A1's core promise at
// the Claude edge: the ChatProvider path (neutral turns -> adapter ->
// converters) produces a wire request BYTE-IDENTICAL to the direct
// AgenticProvider path, over a conversation exercising every block kind, and
// returns the equivalent response. Covers the compat adapter AND the
// concrete provider's direct ChatProvider implementation.
func TestChatAdapter_Claude_RequestBytesIdentical(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", claudeJSONBody)
	defer srv.Close()

	p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	conv := roundTripConversation()
	req := ChatRequest{System: "sys", Turns: TurnsFromConversation(conv), Tools: chatTestTools}
	ctx := context.Background()

	direct, err := p.CompleteWithTools(ctx, conv, chatTestTools, "sys")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	adapted := AsChatProvider(opaqueAgentic{p})
	if _, isAdapter := adapted.(*agenticChatAdapter); !isAdapter {
		t.Fatalf("opaque wrapper should route through the compat adapter")
	}
	viaAdapter, err := adapted.Chat(ctx, req)
	if err != nil {
		t.Fatalf("adapter chat: %v", err)
	}
	viaDirectImpl, err := AsChatProvider(p).Chat(ctx, req)
	if err != nil {
		t.Fatalf("direct chat impl: %v", err)
	}

	if len(bodies) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("adapter request differs from direct request:\n direct  %s\n adapter %s", bodies[0], bodies[1])
	}
	if !bytes.Equal(bodies[0], bodies[2]) {
		t.Errorf("direct ChatProvider impl request differs:\n direct %s\n chat   %s", bodies[0], bodies[2])
	}
	if got := AgenticResponseFromChat(viaAdapter); !reflect.DeepEqual(got, direct) {
		t.Errorf("adapter response:\n want %+v\n  got %+v", direct, got)
	}
	if got := AgenticResponseFromChat(viaDirectImpl); !reflect.DeepEqual(got, direct) {
		t.Errorf("direct-impl response:\n want %+v\n  got %+v", direct, got)
	}
}

// TestChatAdapter_Claude_StreamEventsIdentical proves the streaming contract:
// ChatStream emits the exact same StreamEvent sequence as the direct
// CompleteWithToolsStream call, over the same byte-identical request, and
// assembles the same response.
func TestChatAdapter_Claude_StreamEventsIdentical(t *testing.T) {
	sse := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":9,"output_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-s"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi "}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"there"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"bash","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":2}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	var bodies [][]byte
	srv := captureServer(&bodies, "text/event-stream", sse)
	defer srv.Close()

	p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	conv := roundTripConversation()
	ctx := context.Background()

	var directEvents []StreamEvent
	direct, err := p.CompleteWithToolsStream(ctx, conv, chatTestTools, "sys",
		func(ev StreamEvent) { directEvents = append(directEvents, ev) })
	if err != nil {
		t.Fatalf("direct stream: %v", err)
	}

	adapted := AsChatProvider(opaqueStreamingAgentic{p})
	if !ChatStreamSupported(adapted) {
		t.Fatal("adapter over a streaming provider must report streaming")
	}
	var chatEvents []StreamEvent
	viaChat, err := adapted.ChatStream(ctx,
		ChatRequest{System: "sys", Turns: TurnsFromConversation(conv), Tools: chatTestTools},
		func(ev StreamEvent) { chatEvents = append(chatEvents, ev) })
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}

	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("stream request bodies differ (%d requests)", len(bodies))
	}
	if !reflect.DeepEqual(chatEvents, directEvents) {
		t.Errorf("event sequences differ:\n direct %+v\n chat   %+v", directEvents, chatEvents)
	}
	if got := AgenticResponseFromChat(viaChat); !reflect.DeepEqual(got, direct) {
		t.Errorf("assembled responses differ:\n want %+v\n  got %+v", direct, got)
	}
}

const oaiJSONBody = `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",` +
	`"content":"done","tool_calls":[{"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":21,"completion_tokens":4}}`

// responsesJSONBody is oaiJSONBody's Responses-dialect twin — same text, same
// tool call, same token counts — plus a leading reasoning item, which the
// mapping must skip rather than surface as an empty block.
const responsesJSONBody = `{"id":"resp_1","object":"response","status":"completed","output":[` +
	`{"id":"rs_1","type":"reasoning","summary":[]},` +
	`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done"}]},` +
	`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_9","name":"bash","arguments":"{\"command\":\"ls\"}"}],` +
	`"usage":{"input_tokens":21,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},` +
	`"output_tokens":4,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":25}}`

// TestChatAdapter_OpenAI_RequestBytesIdentical: same byte-identity proof for
// the OpenAI-compatible provider (openai/ollama/gemini dialect).
func TestChatAdapter_OpenAI_RequestBytesIdentical(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", responsesJSONBody)
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-x", 512)
	conv := roundTripConversation()
	ctx := context.Background()

	direct, err := p.CompleteWithTools(ctx, conv, chatTestTools, "sys")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	viaChat, err := AsChatProvider(opaqueAgentic{p}).Chat(ctx,
		ChatRequest{System: "sys", Turns: TurnsFromConversation(conv), Tools: chatTestTools})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("request bodies differ (%d requests):\n direct %s\n chat   %s",
			len(bodies), bodies[0], bodies[len(bodies)-1])
	}
	if got := AgenticResponseFromChat(viaChat); !reflect.DeepEqual(got, direct) {
		t.Errorf("responses differ:\n want %+v\n  got %+v", direct, got)
	}
}

// TestChatAdapter_OpenAI_StreamEventsIdentical: same stream-equivalence proof
// for the OpenAI-compatible streamer.
func TestChatAdapter_OpenAI_StreamEventsIdentical(t *testing.T) {
	sse := fakeOAISSE(
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]},"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7}}`,
		`[DONE]`,
	)
	var bodies [][]byte
	srv := captureServer(&bodies, "text/event-stream", sse)
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3", 256)
	conv := roundTripConversation()
	ctx := context.Background()

	var directEvents []StreamEvent
	direct, err := p.CompleteWithToolsStream(ctx, conv, chatTestTools, "sys",
		func(ev StreamEvent) { directEvents = append(directEvents, ev) })
	if err != nil {
		t.Fatalf("direct stream: %v", err)
	}

	var chatEvents []StreamEvent
	viaChat, err := AsChatProvider(opaqueStreamingAgentic{p}).ChatStream(ctx,
		ChatRequest{System: "sys", Turns: TurnsFromConversation(conv), Tools: chatTestTools},
		func(ev StreamEvent) { chatEvents = append(chatEvents, ev) })
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}

	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("stream request bodies differ (%d requests)", len(bodies))
	}
	if !reflect.DeepEqual(chatEvents, directEvents) {
		t.Errorf("event sequences differ:\n direct %+v\n chat   %+v", directEvents, chatEvents)
	}
	if got := AgenticResponseFromChat(viaChat); !reflect.DeepEqual(got, direct) {
		t.Errorf("assembled responses differ:\n want %+v\n  got %+v", direct, got)
	}
}

// TestChatAdapter_NonStreamingProviderDegrades: adapting a provider without
// StreamingProvider reports no streaming, and ChatStream falls back to the
// one-shot path delivering zero events — the same degradation the shim path
// applies.
func TestChatAdapter_NonStreamingProviderDegrades(t *testing.T) {
	static := &StaticAgenticProvider{Response: "canned"}
	cp := AsChatProvider(static)
	if ChatStreamSupported(cp) {
		t.Error("static provider must not report streaming")
	}
	events := 0
	resp, err := cp.ChatStream(context.Background(),
		ChatRequest{Turns: []Turn{{Role: RoleUser, Blocks: []Block{{Kind: BlockKindText, Text: "hi"}}}}},
		func(StreamEvent) { events++ })
	if err != nil {
		t.Fatalf("chat stream fallback: %v", err)
	}
	if events != 0 {
		t.Errorf("fallback delivered %d events, want 0", events)
	}
	want := &AgenticResponse{TextBlocks: []TextBlock{{Text: "canned"}}, StopReason: StopReasonEndTurn}
	if got := AgenticResponseFromChat(resp); !reflect.DeepEqual(got, want) {
		t.Errorf("fallback response:\n want %+v\n  got %+v", want, got)
	}
	if name := cp.Name(); name != "static" {
		t.Errorf("adapter name = %q", name)
	}
}

// TestChatRequest_ModelOverride: a per-request model override is honored via
// the ModelEffortOverridable seam and errors loudly when the provider lacks
// it. A non-zero MaxTokens on a provider without the MaxTokensOverridable
// seam likewise errors instead of being silently dropped.
func TestChatRequest_ModelOverride(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", claudeJSONBody)
	defer srv.Close()

	p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	ctx := context.Background()
	turns := []Turn{{Role: RoleUser, Blocks: []Block{{Kind: BlockKindText, Text: "hi"}}}}

	if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{Turns: turns, Model: "claude-y"}); err != nil {
		t.Fatalf("model override: %v", err)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil || sent.Model != "claude-y" {
		t.Errorf("request model = %q (err %v), want claude-y", sent.Model, err)
	}

	static := AsChatProvider(&StaticAgenticProvider{Response: "x"})
	if _, err := static.Chat(ctx, ChatRequest{Turns: turns, Model: "other"}); err == nil {
		t.Error("model override on a non-overridable provider must error, not silently drop")
	}
	if _, err := static.Chat(ctx, ChatRequest{Turns: turns, MaxTokens: 100}); err == nil {
		t.Error("MaxTokens on a seam-less provider must error, not silently drop")
	}
}
