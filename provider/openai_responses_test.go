package provider

// Tests for the OpenAI Responses dialect (openai_responses.go). The wire
// contract asserted here was verified against the live API, not read from
// docs, and each assertion below corresponds to something that made rysh fail
// outright when it was wrong: a nested tool shape, the wrong output-cap
// spelling, or a tool result sent as a role message all return 400.
//
// The per-family split is the load-bearing invariant: OpenAI proper speaks
// Responses, while Ollama (air-gapped mode) and the Gemini compat layer speak
// Chat Completions and would 404 on /responses.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// responsesConversation exercises every input-item shape in one turn list:
// user text, an assistant turn with text AND a tool call, a successful tool
// result, and a failed one.
func responsesConversation() []ConversationTurn {
	return []ConversationTurn{
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Content: "running ls", ToolCalls: []ToolCallRequest{
			{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "a.go b.go"},
		{Role: "assistant", ToolCalls: []ToolCallRequest{
			{ID: "call_2", Name: "file_read", Input: nil}, // no args: "{}" on the wire
		}},
		{Role: "tool", ToolCallID: "call_2", Content: "no such file", IsError: true},
	}
}

// TestResponsesRequestShape pins the whole request body against the live wire
// contract, decoded as raw JSON so a change in the Go structs cannot quietly
// redefine what "correct" means.
func TestResponsesRequestShape(t *testing.T) {
	var bodies [][]byte
	var paths []string
	srv := pathCaptureServer(&bodies, &paths, "application/json", responsesJSONBody)
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-5.6-sol", 4096)
	if _, err := p.CompleteWithTools(context.Background(), responsesConversation(), chatTestTools, "you are a terminal agent"); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if paths[0] != "/responses" {
		t.Fatalf("posted to %q, want /responses", paths[0])
	}

	var sent struct {
		Model        string           `json:"model"`
		Instructions string           `json:"instructions"`
		Input        []map[string]any `json:"input"`
		Tools        []map[string]any `json:"tools"`
		MaxOutput    int              `json:"max_output_tokens"`
		Store        *bool            `json:"store"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if sent.Model != "gpt-5.6-sol" || sent.MaxOutput != 4096 {
		t.Errorf("model/cap = %q/%d, want gpt-5.6-sol/4096", sent.Model, sent.MaxOutput)
	}
	// The system prompt is a top-level field here, NOT a message in the input.
	if sent.Instructions != "you are a terminal agent" {
		t.Errorf("instructions = %q", sent.Instructions)
	}
	// store defaults to TRUE server-side: an absent field would silently opt
	// every rysh conversation into retention on OpenAI's side.
	if sent.Store == nil || *sent.Store {
		t.Errorf("store = %v, want an explicit false", sent.Store)
	}

	// Tools are FLAT here — Chat Completions nests these under "function".
	if len(sent.Tools) != 2 {
		t.Fatalf("tools = %+v", sent.Tools)
	}
	if _, nested := sent.Tools[0]["function"]; nested {
		t.Errorf("tool sent in the nested Chat Completions shape: %+v", sent.Tools[0])
	}
	if sent.Tools[0]["type"] != "function" || sent.Tools[0]["name"] != "bash" ||
		sent.Tools[0]["description"] != "run a command" || sent.Tools[0]["parameters"] == nil {
		t.Errorf("tool[0] = %+v", sent.Tools[0])
	}

	// Input items: conversational turns keep role+content; calls and results
	// are bare typed items keyed by call_id.
	want := []map[string]any{
		{"role": "user", "content": "list the files"},
		{"role": "assistant", "content": "running ls"},
		{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": `{"command":"ls"}`},
		{"type": "function_call_output", "call_id": "call_1", "output": "a.go b.go"},
		{"type": "function_call", "call_id": "call_2", "name": "file_read", "arguments": "{}"},
		// IsError has no wire flag in this dialect; folding it into the text is
		// what keeps a failure distinguishable from an empty success.
		{"type": "function_call_output", "call_id": "call_2", "output": "ERROR: no such file"},
	}
	if len(sent.Input) != len(want) {
		t.Fatalf("input items = %d, want %d:\n%s", len(sent.Input), len(want), bodies[0])
	}
	for i, w := range want {
		for k, v := range w {
			if got := sent.Input[i][k]; got != v {
				t.Errorf("input[%d].%s = %v, want %v", i, k, got, v)
			}
		}
	}
}

// TestResponsesResponseMapping covers the reply direction: text, tool calls,
// usage (including the cache counters), and the stop reasons the orchestrator
// branches on. Table-driven over raw wire bodies, mirroring the live shapes.
func TestResponsesResponseMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantText   []string
		wantCalls  int
		wantStop   StopReason
		wantUsage  Usage
		checkCalls func(*testing.T, []ToolCallRequest)
	}{
		{
			name:      "text only ends the turn",
			body:      `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":12,"output_tokens":7}}`,
			wantText:  []string{"hello"},
			wantStop:  StopReasonEndTurn,
			wantUsage: Usage{InputTokens: 12, OutputTokens: 7},
		},
		{
			// This dialect reports "completed" whether or not tools were called,
			// so the presence of a function_call is the ONLY signal that tells
			// the orchestrator to keep looping.
			name:      "function_call drives the tool loop",
			body:      `{"status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"running it"}]},{"type":"function_call","call_id":"call_9","name":"bash","arguments":"{\"command\":\"ls\"}"}],"usage":{"input_tokens":21,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens":4}}`,
			wantText:  []string{"running it"},
			wantCalls: 1,
			wantStop:  StopReasonToolUse,
			wantUsage: Usage{InputTokens: 21, OutputTokens: 4, CacheReadInputTokens: 3, CacheCreationInputTokens: 2},
			checkCalls: func(t *testing.T, calls []ToolCallRequest) {
				if calls[0].ID != "call_9" || calls[0].Name != "bash" || string(calls[0].Input) != `{"command":"ls"}` {
					t.Errorf("tool call = %+v (input %s)", calls[0], calls[0].Input)
				}
			},
		},
		{
			// Arguments arrive as a JSON *string*; an empty one must still be a
			// valid object or it poisons every later request at marshal time.
			name:      "empty arguments become an empty object",
			body:      `{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"noop","arguments":""}]}`,
			wantCalls: 1,
			wantStop:  StopReasonToolUse,
			checkCalls: func(t *testing.T, calls []ToolCallRequest) {
				if string(calls[0].Input) != "{}" || !json.Valid(calls[0].Input) {
					t.Errorf("input = %q, want {}", calls[0].Input)
				}
			},
		},
		{
			// Truncation is a status + reason here, not a finish_reason.
			name:      "incomplete on the cap maps to max tokens",
			body:      `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"cut off"}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":16,"output_tokens":16}}`,
			wantText:  []string{"cut off"},
			wantStop:  StopReasonMaxTokens,
			wantUsage: Usage{InputTokens: 16, OutputTokens: 16},
		},
		{
			name:     "incomplete for another reason is not a cap hit",
			body:     `{"status":"incomplete","output":[],"incomplete_details":{"reason":"content_filter"}}`,
			wantStop: StopReasonEndTurn,
		},
		{
			// Reasoning items carry no client-visible content; surfacing them
			// would add an empty text block to every GPT-5.6 turn.
			name:     "reasoning items are skipped",
			body:     `{"status":"completed","output":[{"type":"reasoning","summary":[]}]}`,
			wantStop: StopReasonEndTurn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out respResponse
			if err := json.Unmarshal([]byte(tc.body), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ar := responsesToAgentic(&out)

			if len(ar.TextBlocks) != len(tc.wantText) {
				t.Fatalf("text blocks = %+v, want %v", ar.TextBlocks, tc.wantText)
			}
			for i, want := range tc.wantText {
				if ar.TextBlocks[i].Text != want {
					t.Errorf("text[%d] = %q, want %q", i, ar.TextBlocks[i].Text, want)
				}
			}
			if len(ar.ToolCalls) != tc.wantCalls {
				t.Fatalf("tool calls = %+v, want %d", ar.ToolCalls, tc.wantCalls)
			}
			if tc.checkCalls != nil {
				tc.checkCalls(t, ar.ToolCalls)
			}
			if ar.StopReason != tc.wantStop {
				t.Errorf("stop = %q, want %q", ar.StopReason, tc.wantStop)
			}
			if ar.Usage != tc.wantUsage {
				t.Errorf("usage = %+v, want %+v", ar.Usage, tc.wantUsage)
			}
		})
	}
}

// TestResponsesToolResultRoundTrip closes the agentic loop at the translation
// layer: the tool call this dialect reports must come back as an input item
// the SAME call_id can be answered against. A mismatch here is what strands a
// tool call mid-loop.
func TestResponsesToolResultRoundTrip(t *testing.T) {
	var out respResponse
	if err := json.Unmarshal([]byte(
		`{"status":"completed","output":[{"type":"function_call","call_id":"call_rt","name":"bash","arguments":"{\"command\":\"pwd\"}"}]}`,
	), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	call := responsesToAgentic(&out).ToolCalls[0]

	// Replay the assistant turn plus its result, exactly as the orchestrator
	// would append them.
	items := turnToResponses(ConversationTurn{Role: "assistant", ToolCalls: []ToolCallRequest{call}})
	items = append(items, turnToResponses(ConversationTurn{Role: "tool", ToolCallID: call.ID, Content: "/home"})...)

	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Type != "function_call" || items[0].CallID != "call_rt" || items[0].Arguments != `{"command":"pwd"}` {
		t.Errorf("call item = %+v", items[0])
	}
	if items[1].Type != "function_call_output" || items[1].CallID != "call_rt" || items[1].Output != "/home" {
		t.Errorf("result item = %+v", items[1])
	}
	// The result must NOT be a role message: this dialect has no "tool" role.
	if items[1].Role != "" {
		t.Errorf("result item carried role %q; it must be a bare typed item", items[1].Role)
	}
}

// TestResponsesDialectRouting pins WHICH endpoint each provider family talks
// to, on all four paths (shim/native × streaming/non-streaming). Routing one
// path differently from another is the exact bug the differential tests exist
// to catch, and sending Responses to Ollama would 404 air-gapped mode.
func TestResponsesDialectRouting(t *testing.T) {
	sse := "data: " + `{"type":"response.completed","response":{"status":"completed","output":[],"usage":{}}}` + "\n\n"

	for _, tc := range []struct {
		family string
		want   string
		reply  string // a reply in the dialect that family speaks
	}{
		{"openai", "/responses", responsesJSONBody},
		{"ollama", "/chat/completions", oaiJSONBody},
		{"gemini", "/chat/completions", oaiJSONBody},
	} {
		t.Run(tc.family, func(t *testing.T) {
			// Non-streaming: both paths.
			var bodies [][]byte
			var paths []string
			srv := pathCaptureServer(&bodies, &paths, "application/json", tc.reply)
			defer srv.Close()

			p := NewOpenAIAgenticProvider(tc.family, "k", srv.URL, "m", 128)
			ctx := context.Background()
			if _, err := p.CompleteWithTools(ctx, responsesConversation(), chatTestTools, "sys"); err != nil {
				t.Fatalf("shim: %v", err)
			}
			if _, err := AsChatProvider(p).Chat(ctx, ChatRequest{System: "sys", Turns: TurnsFromConversation(responsesConversation()), Tools: chatTestTools}); err != nil {
				t.Fatalf("native: %v", err)
			}

			// Streaming: both paths. The Chat Completions families get an SSE
			// body they cannot parse, which is fine — only the path matters,
			// and the parse error is not what is under test.
			var streamBodies [][]byte
			var streamPaths []string
			streamSrv := pathCaptureServer(&streamBodies, &streamPaths, "text/event-stream", sse)
			defer streamSrv.Close()

			sp := NewOpenAIAgenticProvider(tc.family, "k", streamSrv.URL, "m", 128)
			_, _ = sp.CompleteWithToolsStream(ctx, responsesConversation(), chatTestTools, "sys", nil)
			_, _ = AsChatProvider(sp).ChatStream(ctx, ChatRequest{System: "sys", Turns: TurnsFromConversation(responsesConversation()), Tools: chatTestTools}, nil)

			all := append(paths, streamPaths...)
			if len(all) != 4 {
				t.Fatalf("%s made %d requests, want 4 (shim/native × streaming/not)", tc.family, len(all))
			}
			for i, got := range all {
				if got != tc.want {
					t.Errorf("%s request %d posted to %q, want %q", tc.family, i, got, tc.want)
				}
			}
		})
	}
}

// TestResponsesRequestOmitsChatCompletionsFields guards the negative half of
// the split: current OpenAI models reject BOTH Chat Completions cap spellings
// on /responses, and a system message in the input is not the same thing as
// instructions.
func TestResponsesRequestOmitsChatCompletionsFields(t *testing.T) {
	var bodies [][]byte
	srv := captureServer(&bodies, "application/json", responsesJSONBody)
	defer srv.Close()

	p := NewOpenAIAgenticProvider("openai", "k", srv.URL, "gpt-5.6-sol", 999)
	if _, err := p.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "sys"); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}

	body := string(bodies[0])
	for _, forbidden := range []string{`"max_tokens"`, `"max_completion_tokens"`, `"messages"`, `"stream_options"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Responses body carries Chat Completions field %s:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"max_output_tokens":999`) {
		t.Errorf("output cap missing from body:\n%s", body)
	}
}
