package provider

// Differential tests for the native ChatProvider implementations (design 002
// A1 final step). THE ACCEPTANCE BAR: for identical ChatRequests, the native
// path (bare provider fast-path) must produce wire request bodies
// BYTE-IDENTICAL to the compat-adapter path (ConversationFromTurns -> shim
// builders), identical StreamEvent sequences, and identical ChatResponses —
// including the documented turns.go canonicalizations (C1/C2), the healing
// chokepoints, and the Model/MaxTokens override seams. Both paths run against
// the same httptest server and the raw bodies/events/responses are diffed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// fullSeamAgentic hides the native ChatProvider implementation (forcing
// AsChatProvider onto the compat adapter) while forwarding streaming and both
// per-request override seams — the shape of a fully-featured decorator.
type fullSeamAgentic struct{ AgenticProvider }

func (w fullSeamAgentic) CompleteWithToolsStream(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	cb StreamCallback,
) (*AgenticResponse, error) {
	return w.AgenticProvider.(StreamingProvider).CompleteWithToolsStream(ctx, conversation, tools, systemPrompt, cb)
}

func (w fullSeamAgentic) WithModelEffort(model, effort string) AgenticProvider {
	return w.AgenticProvider.(ModelEffortOverridable).WithModelEffort(model, effort)
}

func (w fullSeamAgentic) WithMaxTokens(maxTokens int) AgenticProvider {
	return w.AgenticProvider.(MaxTokensOverridable).WithMaxTokens(maxTokens)
}

// nativeOnlyTurns are neutral shapes NO shim conversation can produce —
// multi-result tool turns, results embedded in assistant turns, text after
// tool calls, degenerate nil payloads — plus healing triggers (orphaned
// results, unanswered calls). The native builder must canonicalize them onto
// the wire exactly as the adapter path (fan-out + shim builders) does.
func nativeOnlyTurns() []Turn {
	img := &ImageSource{Type: "base64", MediaType: "image/png", Data: "aGk="}
	return []Turn{
		// Leading assistant turn: trimmed by the head heal on both paths.
		{Role: RoleAssistant, Blocks: []Block{{Kind: BlockKindText, Text: "dangling head"}}},
		{Role: RoleUser, Blocks: []Block{{Kind: BlockKindText, Text: "hello"}}},
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockKindThinking, Thinking: &ThinkingBlock{Text: "plan", Signature: "sig-1"}},
			{Kind: BlockKindText, Text: "running two tools"},
			{Kind: BlockKindToolCall, ToolCall: &ToolCall{ID: "c1", Name: "bash", ParamsJSON: json.RawMessage(`{"command":"ls"}`)}},
			{Kind: BlockKindToolCall, ToolCall: &ToolCall{ID: "c2", Name: "file_read", ParamsJSON: json.RawMessage(`{"path":"/x"}`)}},
			{Kind: BlockKindToolCall, ToolCall: nil}, // degenerate: skipped
		}},
		// ONE tool turn carrying BOTH results (fan-out shape), with an image
		// attachment following the first result and text after the second,
		// plus an orphaned third result the pairing heal must drop.
		{Role: RoleTool, Blocks: []Block{
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "total 0"}},
			{Kind: BlockKindImage, Image: img},
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c2", Content: "[error kind=not_found]", IsError: true}},
			{Kind: BlockKindText, Text: "capture note"},
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c9-orphan", Content: "dropped"}},
		}},
		// Assistant turn with an UNANSWERED tool call: the pairing heal must
		// synthesize the missing result on both paths.
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockKindToolCall, ToolCall: &ToolCall{ID: "c3", Name: "bash", ParamsJSON: json.RawMessage(`{"command":"pwd"}`)}},
		}},
		// Assistant answer with text AFTER a tool call (no shim equivalent),
		// an invalid-JSON tool input (sanitized on the wire), and a result
		// EMBEDDED in the assistant turn (fans out on the adapter path).
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockKindToolCall, ToolCall: &ToolCall{ID: "c4", Name: "bash", ParamsJSON: json.RawMessage(`{"broken":`)}},
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c4", Content: "ok"}},
			{Kind: BlockKindText, Text: "text after the call"},
		}},
		// Unknown role: dropped from the Claude wire, user-role on OpenAI.
		{Role: TurnRole("system-note"), Blocks: []Block{{Kind: BlockKindText, Text: "synthetic summary"}}},
		// User turn with image-only content, a degenerate nil-source image,
		// an unknown block kind (C2: reads as text), and a nil thinking ptr.
		{Role: RoleUser, Blocks: []Block{
			{Kind: BlockKindImage, Image: img},
			{Kind: BlockKindImage, Image: nil},
			{Kind: BlockKind("mystery"), Text: "odd"},
			{Kind: BlockKindThinking, Thinking: nil},
		}},
		// Tool turn with NO result block at all (degenerate; orphan-dropped).
		{Role: RoleTool, Blocks: []Block{{Kind: BlockKindText, Text: "stray"}}},
		// Empty user turn.
		{Role: RoleUser},
	}
}

// diffChat runs req through both paths against the same capture server and
// asserts byte-identical bodies and deeply-equal responses. native must be
// the fast-pathed bare provider, adapter the compat adapter over a seam-
// forwarding wrapper of the same provider.
func diffChat(t *testing.T, bodies *[][]byte, native, adapter ChatProvider, req ChatRequest) {
	t.Helper()
	*bodies = (*bodies)[:0]
	nResp, err := native.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("native chat: %v", err)
	}
	aResp, err := adapter.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("adapter chat: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*bodies))
	}
	if !bytes.Equal((*bodies)[0], (*bodies)[1]) {
		t.Errorf("wire bodies differ:\n native  %s\n adapter %s", (*bodies)[0], (*bodies)[1])
	}
	if !reflect.DeepEqual(nResp, aResp) {
		t.Errorf("responses differ:\n native  %+v\n adapter %+v", nResp, aResp)
	}
}

// diffChatStream is diffChat for the streaming path, additionally asserting
// identical StreamEvent sequences.
func diffChatStream(t *testing.T, bodies *[][]byte, native, adapter ChatProvider, req ChatRequest) {
	t.Helper()
	*bodies = (*bodies)[:0]
	var nEvents, aEvents []StreamEvent
	nResp, err := native.ChatStream(context.Background(), req, func(ev StreamEvent) { nEvents = append(nEvents, ev) })
	if err != nil {
		t.Fatalf("native chat stream: %v", err)
	}
	aResp, err := adapter.ChatStream(context.Background(), req, func(ev StreamEvent) { aEvents = append(aEvents, ev) })
	if err != nil {
		t.Fatalf("adapter chat stream: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*bodies))
	}
	if !bytes.Equal((*bodies)[0], (*bodies)[1]) {
		t.Errorf("stream wire bodies differ:\n native  %s\n adapter %s", (*bodies)[0], (*bodies)[1])
	}
	if !reflect.DeepEqual(nEvents, aEvents) {
		t.Errorf("event sequences differ:\n native  %+v\n adapter %+v", nEvents, aEvents)
	}
	if !reflect.DeepEqual(nResp, aResp) {
		t.Errorf("assembled responses differ:\n native  %+v\n adapter %+v", nResp, aResp)
	}
}

// mustBeNative asserts AsChatProvider fast-pathed to the bare provider.
func mustBeNative(t *testing.T, p AgenticProvider) ChatProvider {
	t.Helper()
	cp := AsChatProvider(p)
	if any(cp) != any(p) {
		t.Fatalf("AsChatProvider(%T) = %T, want the fast-pathed provider itself", p, cp)
	}
	return cp
}

// mustBeAdapter asserts AsChatProvider kept p on the compat-adapter path.
func mustBeAdapter(t *testing.T, p AgenticProvider) ChatProvider {
	t.Helper()
	cp := AsChatProvider(p)
	if _, ok := cp.(*agenticChatAdapter); !ok {
		t.Fatalf("AsChatProvider(%T) = %T, want the compat adapter", p, cp)
	}
	return cp
}

// chatRequestVariants are the override combinations every differential run
// covers: none, Model, MaxTokens, both.
func chatRequestVariants(turns []Turn, tools []ToolSpec) map[string]ChatRequest {
	return map[string]ChatRequest{
		"plain":           {System: "sys", Turns: turns, Tools: tools},
		"model-override":  {System: "sys", Turns: turns, Tools: tools, Model: "claude-y"},
		"max-tokens":      {System: "sys", Turns: turns, Tools: tools, MaxTokens: 777},
		"model+maxtokens": {System: "sys", Turns: turns, Tools: tools, Model: "claude-y", MaxTokens: 999},
		"no-system":       {Turns: turns, Tools: tools},
	}
}

// TestNativeChat_Claude_Differential: byte-identical wire bodies and
// identical responses across shim-expressible AND native-only shapes, with
// caching on/off, thinking+effort configured, and every override variant.
func TestNativeChat_Claude_Differential(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", claudeJSONBody)
	defer srv.Close()

	corpora := map[string][]Turn{
		"shim-shapes":   TurnsFromConversation(roundTripConversation()),
		"native-shapes": nativeOnlyTurns(),
		"empty":         nil,
	}
	configure := map[string]func(*ClaudeAgenticProvider){
		"default":  func(*ClaudeAgenticProvider) {},
		"no-cache": func(p *ClaudeAgenticProvider) { p.SetCacheEnabled(false) },
		"thinking-effort": func(p *ClaudeAgenticProvider) {
			p.SetThinking(&ThinkingConfig{Type: "enabled", BudgetTokens: 1024})
			p.effort = "high"
		},
	}

	for cfgName, cfg := range configure {
		for corpusName, turns := range corpora {
			for variant, req := range chatRequestVariants(turns, chatTestTools) {
				t.Run(cfgName+"/"+corpusName+"/"+variant, func(t *testing.T) {
					p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
					cfg(p)
					diffChat(t, &bodies, mustBeNative(t, p), mustBeAdapter(t, fullSeamAgentic{p}), req)
				})
			}
		}
	}
}

// TestNativeChatStream_Claude_Differential: same equivalence on the streaming
// path — bodies, StreamEvent sequences, assembled responses.
func TestNativeChatStream_Claude_Differential(t *testing.T) {
	sse := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-s"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi "}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"there"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"bash","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":2}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	var bodies [][]byte
	srv := captureServer(&bodies, "text/event-stream", sse)
	defer srv.Close()

	for corpusName, turns := range map[string][]Turn{
		"shim-shapes":   TurnsFromConversation(roundTripConversation()),
		"native-shapes": nativeOnlyTurns(),
	} {
		for variant, req := range chatRequestVariants(turns, chatTestTools) {
			t.Run(corpusName+"/"+variant, func(t *testing.T) {
				p := NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
				diffChatStream(t, &bodies, mustBeNative(t, p), mustBeAdapter(t, fullSeamAgentic{p}), req)
			})
		}
	}
}

// TestNativeChat_OpenAI_Differential: byte-identical wire bodies and
// identical responses for the OpenAI-compatible dialect.
func TestNativeChat_OpenAI_Differential(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", oaiJSONBody)
	defer srv.Close()

	for corpusName, turns := range map[string][]Turn{
		"shim-shapes":   TurnsFromConversation(roundTripConversation()),
		"native-shapes": nativeOnlyTurns(),
		"empty":         nil,
	} {
		for variant, req := range chatRequestVariants(turns, chatTestTools) {
			t.Run(corpusName+"/"+variant, func(t *testing.T) {
				p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-x", 512)
				diffChat(t, &bodies, mustBeNative(t, p), mustBeAdapter(t, fullSeamAgentic{p}), req)
			})
		}
	}
}

// TestNativeChatStream_OpenAI_Differential: same equivalence on the OpenAI
// streaming path.
func TestNativeChatStream_OpenAI_Differential(t *testing.T) {
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

	for corpusName, turns := range map[string][]Turn{
		"shim-shapes":   TurnsFromConversation(roundTripConversation()),
		"native-shapes": nativeOnlyTurns(),
	} {
		for variant, req := range chatRequestVariants(turns, chatTestTools) {
			t.Run(corpusName+"/"+variant, func(t *testing.T) {
				p := NewOpenAIAgenticProvider("ollama", "", srv.URL, "llama3", 256)
				diffChatStream(t, &bodies, mustBeNative(t, p), mustBeAdapter(t, fullSeamAgentic{p}), req)
			})
		}
	}
}

// TestNativeChat_Differential_Randomized is the drift net: seeded random
// conversations over ARBITRARY neutral shapes (any block kind in any order,
// nil payloads, orphaned/duplicate/unanswered tool pairings, unknown roles
// and block kinds, invalid tool-input JSON) must produce byte-identical wire
// bodies through both paths, for both providers.
func TestNativeChat_Differential_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	var claudeBodies, oaiBodies [][]byte
	claudeSrv := captureServer(&claudeBodies, "application/json", claudeJSONBody)
	defer claudeSrv.Close()
	oaiSrv := captureServer(&oaiBodies, "application/json", oaiJSONBody)
	defer oaiSrv.Close()

	claude := NewClaudeAgenticProvider("k", claudeSrv.URL, "claude-x", 0)
	oai := NewOpenAIAgenticProvider("openai", "k", oaiSrv.URL, "gpt-x", 512)

	for trial := 0; trial < 150; trial++ {
		turns := randomNeutralTurns(rng)
		req := ChatRequest{System: "sys", Turns: turns, Tools: chatTestTools}
		if rng.Intn(3) == 0 {
			req.System = ""
		}
		t.Run(fmt.Sprintf("trial-%d", trial), func(t *testing.T) {
			diffChat(t, &claudeBodies, mustBeNative(t, claude), mustBeAdapter(t, fullSeamAgentic{claude}), req)
			diffChat(t, &oaiBodies, mustBeNative(t, oai), mustBeAdapter(t, fullSeamAgentic{oai}), req)
		})
		if t.Failed() {
			t.Fatalf("trial %d turns: %+v", trial, turns)
		}
	}
}

// randomNeutralTurns generates arbitrary neutral conversations, deliberately
// unconstrained by shim invariants.
func randomNeutralTurns(rng *rand.Rand) []Turn {
	roles := []TurnRole{RoleUser, RoleAssistant, RoleTool, TurnRole("weird-role")}
	params := []json.RawMessage{
		json.RawMessage(`{"a":1}`),
		json.RawMessage(`{"broken":`), // invalid: sanitized on the Claude wire
		nil,                           // empty: sanitized on the Claude wire
		json.RawMessage(`  {"b":"x"} `),
	}
	images := []*ImageSource{
		{Type: "base64", MediaType: "image/png", Data: "aGk="},
		{Type: "base64", MediaType: "image/jpeg", Data: "data:image/png;base64,aGk="}, // healed on the Claude wire
		{Type: "url", URL: "https://example.com/x.png"},
		{Type: "file_id", FileID: "file_1"},
		nil,
	}
	callID := func() string { return fmt.Sprintf("call_%d", rng.Intn(4)) }

	var turns []Turn
	for i := rng.Intn(7); i > 0; i-- {
		turn := Turn{Role: roles[rng.Intn(len(roles))]}
		for j := rng.Intn(6); j > 0; j-- {
			switch rng.Intn(7) {
			case 0:
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindText, Text: fmt.Sprintf("txt-%d", rng.Intn(100))})
			case 1:
				tb := &ThinkingBlock{Text: fmt.Sprintf("think-%d", rng.Intn(100)), Signature: fmt.Sprintf("sig-%d", rng.Intn(100))}
				if rng.Intn(5) == 0 {
					tb = nil // degenerate
				} else if rng.Intn(5) == 0 {
					tb = &ThinkingBlock{} // empty text+signature: skipped on the wire
				}
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindThinking, Thinking: tb})
			case 2:
				tc := &ToolCall{ID: callID(), Name: "bash", ParamsJSON: params[rng.Intn(len(params))]}
				if rng.Intn(6) == 0 {
					tc = nil
				}
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindToolCall, ToolCall: tc})
			case 3:
				tr := &ToolResult{CallID: callID(), Content: fmt.Sprintf("res-%d", rng.Intn(100)), IsError: rng.Intn(2) == 0}
				if rng.Intn(6) == 0 {
					tr = nil
				}
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindToolResult, ToolResult: tr})
			case 4:
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindImage, Image: images[rng.Intn(len(images))]})
			case 5:
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKind("mystery"), Text: fmt.Sprintf("m-%d", rng.Intn(100))})
			case 6:
				// Empty text block (assistant Content=="" shape).
				turn.Blocks = append(turn.Blocks, Block{Kind: BlockKindText})
			}
		}
		turns = append(turns, turn)
	}
	return turns
}

// TestAsChatProvider_FastPathGuard pins the fast-path contract: the two bare
// native providers fast-path; every wrapper shape — interface-holding
// decorators AND structs that EMBED a native provider (inheriting its
// ChatProvider methods by promotion) — stays on the compat adapter.
func TestAsChatProvider_FastPathGuard(t *testing.T) {
	claude := NewClaudeAgenticProvider("k", "http://x", "m", 0)
	oai := NewOpenAIAgenticProvider("openai", "k", "http://x", "m", 0)

	mustBeNative(t, claude)
	mustBeNative(t, oai)
	mustBeAdapter(t, opaqueAgentic{claude})
	mustBeAdapter(t, fullSeamAgentic{claude})
	mustBeAdapter(t, &StaticAgenticProvider{Response: "x"})
	mustBeAdapter(t, &embeddingWrapper{ClaudeAgenticProvider: claude})
}

// embeddingWrapper is the dangerous decorator shape: it EMBEDS the concrete
// provider, so it satisfies ChatProvider (and the native marker) through
// method promotion, while overriding CompleteWithTools with its own
// semantics. If AsChatProvider fast-pathed it, Chat would hit the embedded
// provider directly and silently bypass the override.
type embeddingWrapper struct {
	*ClaudeAgenticProvider
	sawCall bool
}

// CompleteWithTools stands in for any decorator semantics (sanitization,
// defaults, auditing): it redacts a marker string before delegating.
func (w *embeddingWrapper) CompleteWithTools(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
) (*AgenticResponse, error) {
	w.sawCall = true
	redacted := make([]ConversationTurn, len(conversation))
	copy(redacted, conversation)
	for i := range redacted {
		redacted[i].Content = strings.ReplaceAll(redacted[i].Content, "SECRET", "[redacted]")
	}
	return w.ClaudeAgenticProvider.CompleteWithTools(ctx, redacted, tools, strings.ReplaceAll(systemPrompt, "SECRET", "[redacted]"))
}

// TestAsChatProvider_EmbeddingWrapperStaysDecorated proves the guard end to
// end: Chat through an embedding wrapper must route via the wrapper's
// CompleteWithTools override (adapter path), so its decoration reaches the
// wire. Without the identity check this test fails — the promoted native
// Chat would send the un-redacted turns.
func TestAsChatProvider_EmbeddingWrapperStaysDecorated(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", claudeJSONBody)
	defer srv.Close()

	w := &embeddingWrapper{ClaudeAgenticProvider: NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)}
	cp := mustBeAdapter(t, w)

	req := ChatRequest{
		System: "system holds SECRET",
		Turns:  []Turn{{Role: RoleUser, Blocks: []Block{{Kind: BlockKindText, Text: "user holds SECRET"}}}},
	}
	if _, err := cp.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !w.sawCall {
		t.Fatal("wrapper override was bypassed — the fast-path leaked through the embedding")
	}
	body := string(bodies[0])
	if strings.Contains(body, "SECRET") {
		t.Fatalf("un-redacted content reached the wire: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("redaction marker missing from the wire: %s", body)
	}
}
