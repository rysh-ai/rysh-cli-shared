// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"encoding/json"
	"testing"
)

// subAgentConv is a parent conversation whose sub_agent result turn carries a
// two-level SubAgentCalls record.
func subAgentConv(withCalls bool) []ConversationTurn {
	res := ConversationTurn{Role: "tool", ToolCallID: "sa1", Content: "Sub-agent completed successfully.\n\nFinal answer:\ndone\n"}
	if withCalls {
		res.SubAgentCalls = []SubAgentCall{
			{ID: "c1", Name: "bash", Title: "bash ls", Ended: true},
			{ID: "c2", Name: "sub_agent", Title: "inner", Ended: true, Calls: []SubAgentCall{
				{ID: "g1", Name: "read_file", Title: "read a.go", Ended: true, IsError: true},
			}},
		}
	}
	return []ConversationTurn{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []ToolCallRequest{{ID: "sa1", Name: "sub_agent", Input: json.RawMessage(`{"task":"x"}`)}}},
		res,
	}
}

// SubAgentCalls is an audit record only: every provider request rendered
// from the conversation must be BYTE-IDENTICAL with and without it.
func TestSubAgentCalls_NeverReachTheModel(t *testing.T) {
	mustJSON := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	c := &ClaudeAgenticProvider{}
	o := NewOpenAIAgenticProvider("openai", "k", "http://127.0.0.1:9", "gpt-x", 512)
	render := map[string]func([]ConversationTurn) []byte{
		"claude messages":  func(cv []ConversationTurn) []byte { return mustJSON(c.buildMessages(cv)) },
		"openai chat":      func(cv []ConversationTurn) []byte { return mustJSON(o.buildRequest(cv, nil, "sys")) },
		"openai responses": func(cv []ConversationTurn) []byte { return mustJSON(o.buildResponsesRequest(cv, nil, "sys")) },
	}
	for name, r := range render {
		before, after := r(subAgentConv(false)), r(subAgentConv(true))
		if !bytes.Equal(before, after) {
			t.Errorf("%s: request changed by SubAgentCalls\nbefore %s\nafter  %s", name, before, after)
		}
		if bytes.Contains(after, []byte("read a.go")) || bytes.Contains(after, []byte(`"g1"`)) {
			t.Errorf("%s: a sub-agent call leaked into the request: %s", name, after)
		}
	}
}

// SubAgentCalls is in-process only: marshalling a turn (session KV, restore,
// ##hop fork, anything JSON) yields no sub_agent_calls key and none of its data.
func TestSubAgentCalls_NeverMarshalled(t *testing.T) {
	for _, turn := range subAgentConv(true) {
		b, err := json.Marshal(turn)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("sub_agent_calls")) || bytes.Contains(b, []byte("read a.go")) {
			t.Fatalf("SubAgentCalls marshalled: %s", b)
		}
	}
	b, err := json.Marshal(subAgentConv(true))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(subAgentConv(false))
	if !bytes.Equal(b, plain) {
		t.Fatalf("conversation JSON differs with SubAgentCalls set:\n%s\n%s", b, plain)
	}
}
