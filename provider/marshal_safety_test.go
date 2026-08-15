// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"testing"
)

// These tests pin the marshal-safety guards at the two request chokepoints:
// buildMessages (replayed tool_use inputs) and buildToolDefs (input_schema).
//
// Background: json.RawMessage.MarshalJSON fails on empty bytes with
// "unexpected end of JSON input" and on truncated JSON with a similar
// compaction error. A conversation persisted to session KV with a truncated
// tool input (stream died mid input_json_delta) bricked every subsequent
// request with:
//
//	claude-agentic stream: marshal request: json: error calling MarshalJSON
//	for type json.RawMessage: unexpected end of JSON input
//
// The guards substitute the empty object so a poisoned conversation heals on
// the next request instead of failing forever.

func TestSanitizeToolInput(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil", nil, `{}`},
		{"empty", json.RawMessage(``), `{}`},
		{"whitespace", json.RawMessage("  \n"), `{}`},
		{"truncated", json.RawMessage(`{"action":"navi`), `{}`},
		{"doubled", json.RawMessage(`{}{"a":1}`), `{}`},
		{"valid", json.RawMessage(`{"action":"navigate","url":"https://x.dev"}`), `{"action":"navigate","url":"https://x.dev"}`},
		{"valid-empty-object", json.RawMessage(`{}`), `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeToolInput(tc.in)
			if string(got) != tc.want {
				t.Errorf("sanitizeToolInput(%q) = %q, want %q", string(tc.in), string(got), tc.want)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Errorf("sanitized input still fails to marshal: %v", err)
			}
		})
	}
}

// TestBuildMessages_HealsPoisonedToolInput replays a conversation whose
// assistant turn carries a truncated tool_use input (as persisted by a
// pre-fix build) and asserts the assembled request body marshals cleanly.
func TestBuildMessages_HealsPoisonedToolInput(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	conversation := []ConversationTurn{
		{Role: "user", Content: "run the automation"},
		{Role: "assistant", Content: "on it", ToolCalls: []ToolCallRequest{
			{ID: "toolu_1", Name: "browser_action", Input: json.RawMessage(`{"action":"navi`)},
			{ID: "toolu_2", Name: "page_context", Input: json.RawMessage(``)},
		}},
		{Role: "tool", ToolCallID: "toolu_1", Content: "interrupted"},
	}
	messages := c.buildMessages(conversation)
	body := agenticRequest{
		Model:     "claude-test",
		MaxTokens: 16,
		Messages:  messages,
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("request with healed conversation failed to marshal: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("marshalled request is not valid JSON")
	}
}

// TestBuildToolDefs_EmptySchemaFallsBack asserts that a tool registered with
// no (or an invalid) parameter schema gets the required empty-object
// input_schema instead of failing the whole request at marshal time
// (input_schema has no omitempty — it is required by the API).
func TestBuildToolDefs_EmptySchemaFallsBack(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	defs := c.buildToolDefs([]ToolSpec{
		{Name: "no_schema", Description: "d"},
		{Name: "empty_schema", Description: "d", Parameters: json.RawMessage(``)},
		{Name: "bad_schema", Description: "d", Parameters: json.RawMessage(`{"type":`)},
		{Name: "good_schema", Description: "d", Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
	})
	if len(defs) != 4 {
		t.Fatalf("expected 4 defs, got %d", len(defs))
	}
	for _, d := range defs[:3] {
		if string(d.InputSchema) != string(emptyObjectSchema) {
			t.Errorf("%s: input_schema = %q, want empty-object fallback", d.Name, string(d.InputSchema))
		}
	}
	if string(defs[3].InputSchema) != `{"type":"object","properties":{"x":{"type":"string"}}}` {
		t.Errorf("good_schema was rewritten: %q", string(defs[3].InputSchema))
	}
	if _, err := json.Marshal(defs); err != nil {
		t.Fatalf("tool defs failed to marshal: %v", err)
	}
}
