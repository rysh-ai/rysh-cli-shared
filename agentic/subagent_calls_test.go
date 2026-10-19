// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

func asst(calls ...provider.ToolCallRequest) provider.ConversationTurn {
	return provider.ConversationTurn{Role: "assistant", ToolCalls: calls}
}

func call(id, name, input string) provider.ToolCallRequest {
	return provider.ToolCallRequest{ID: id, Name: name, Input: json.RawMessage(input)}
}

// Depth 2: a child that ran bash, then a nested sub_agent (which itself ran
// read_file and stopped mid-write_file), then a failing tool, then stopped
// mid-tool with a second call closed by CloseDanglingToolCalls. The list is in
// call order, ended/is_error/interrupted come from the result turns, and the
// nested calls and orchestrator id ride the nested sub_agent's result turn.
func TestSubAgentCallsFromConversation_Recursive(t *testing.T) {
	grand := []provider.SubAgentCall{
		{ID: "g1", Name: "read_file", Title: stepTitle("read_file", json.RawMessage(`{"path":"a.go"}`)), Ended: true},
		{ID: "g2", Name: "write_file", Title: stepTitle("write_file", json.RawMessage(`{"path":"b.go"}`))},
	}
	conv := []provider.ConversationTurn{
		{Role: "user", Content: "task"},
		asst(call("c1", "bash", `{"command":"ls"}`)),
		{Role: "tool", ToolCallID: "c1", Content: "a.go"},
		asst(call("c2", SubAgentToolName, `{"task":"inner"}`), call("c3", "bash", `{"command":"false"}`)),
		{Role: "tool", ToolCallID: "c2", Content: "summary", Origin: "grandchild-orch", SubAgentCalls: grand},
		{Role: "tool", ToolCallID: "c3", Content: "exit 1", IsError: true},
		asst(call("c4", "bash", `{"command":"sleep 9"}`), call("c5", "bash", `{"command":"true"}`)),
		{Role: "tool", ToolCallID: "c5", Content: "[error kind=cancelled] interrupted", IsError: true, Origin: "bash"},
	}
	got := SubAgentCallsFromConversation(conv)
	want := []provider.SubAgentCall{
		{ID: "c1", Name: "bash", Title: stepTitle("bash", json.RawMessage(`{"command":"ls"}`)), Ended: true},
		{ID: "c2", Name: SubAgentToolName, Title: stepTitle(SubAgentToolName, json.RawMessage(`{"task":"inner"}`)), Ended: true, OrchestratorID: "grandchild-orch", Calls: grand},
		{ID: "c3", Name: "bash", Title: stepTitle("bash", json.RawMessage(`{"command":"false"}`)), Ended: true, IsError: true},
		{ID: "c4", Name: "bash", Title: stepTitle("bash", json.RawMessage(`{"command":"sleep 9"}`))},
		{ID: "c5", Name: "bash", Title: stepTitle("bash", json.RawMessage(`{"command":"true"}`)), Ended: true, IsError: true, Interrupted: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %+v\nwant %+v", got, want)
	}
	if SubAgentCallsFromConversation(nil) != nil {
		t.Fatal("empty conversation must give no calls")
	}
}

// The sub_agent result turn carries the calls in SubAgentCalls, and its
// Content (what the model sees) is BYTE-IDENTICAL to the result without them.
func TestSubAgentResultTurn_CallsInMetadataContentUnchanged(t *testing.T) {
	done := &msg.MsgOrchestratorDone{OrchestratorID: "child-1", Success: true, Summary: "ok",
		Conversation: []provider.ConversationTurn{
			{Role: "user", Content: "task"},
			asst(call("c1", "bash", `{"command":"ls"}`)),
			{Role: "tool", ToolCallID: "c1", Content: "a.go"},
			{Role: "assistant", Content: "found a.go"},
		}}
	calls := SubAgentCallsFromConversation(done.Conversation)
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"sub_agent_id": "child-1", "sub_agent_title": "t", "sub_agent_digest": "d"}
	withMeta := map[string]string{SubAgentCallsMetaKey: string(raw)}
	for k, v := range base {
		withMeta[k] = v
	}
	summary := FormatSubAgentSummary(done)
	tc := call("sa1", SubAgentToolName, `{"task":"x"}`)
	o := &OrchestratorActor{}
	before := o.buildToolResultTurn(tc, &tools.ToolOutput{Content: summary, Metadata: base})
	after := o.buildToolResultTurn(tc, &tools.ToolOutput{Content: summary, Metadata: withMeta})

	if before.Content != after.Content || after.Content != summary {
		t.Fatalf("model-visible content changed:\nbefore %q\nafter  %q", before.Content, after.Content)
	}
	if !reflect.DeepEqual(after.SubAgentCalls, calls) || before.SubAgentCalls != nil {
		t.Fatalf("SubAgentCalls: before %+v after %+v want %+v", before.SubAgentCalls, after.SubAgentCalls, calls)
	}
	after.SubAgentCalls, after.TimestampMs, before.TimestampMs = nil, 0, 0
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("turn differs beyond SubAgentCalls:\nbefore %+v\nafter  %+v", before, after)
	}
	// The pre-existing summary text is unchanged (golden).
	const golden = "Sub-agent completed successfully.\n\nSummary:\nok\n\nFinal answer:\nfound a.go\n"
	if summary != golden {
		t.Fatalf("FormatSubAgentSummary changed: %q", summary)
	}
}

// After the move into SubAgentCalls the Metadata key is gone: neither the
// ToolOutput as it leaves the orchestrator nor the turn marshals the list.
func TestSubAgentCallsMetaKeyDeletedAfterMove(t *testing.T) {
	done := &msg.MsgOrchestratorDone{OrchestratorID: "child-1", Success: true,
		Conversation: []provider.ConversationTurn{
			{Role: "user", Content: "task"},
			asst(call("c1", "bash", `{"command":"secret-target"}`)),
			{Role: "tool", ToolCallID: "c1", Content: "ok"},
		}}
	raw, err := json.Marshal(SubAgentCallsFromConversation(done.Conversation))
	if err != nil {
		t.Fatal(err)
	}
	out := &tools.ToolOutput{Content: FormatSubAgentSummary(done), Metadata: map[string]string{
		"sub_agent_id": "child-1", SubAgentCallsMetaKey: string(raw),
	}}
	o := &OrchestratorActor{}
	turn := o.buildToolResultTurn(call("sa1", SubAgentToolName, `{"task":"x"}`), out)
	if len(turn.SubAgentCalls) != 1 || turn.SubAgentCalls[0].ID != "c1" {
		t.Fatalf("calls not moved onto the turn: %+v", turn.SubAgentCalls)
	}
	if _, ok := out.Metadata[SubAgentCallsMetaKey]; ok {
		t.Fatal("Metadata key still present after the move")
	}
	if out.Metadata["sub_agent_id"] != "child-1" {
		t.Fatal("other metadata must stay")
	}
	for name, v := range map[string]any{"ToolOutput": out, "ConversationTurn": turn} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), SubAgentCallsMetaKey) || strings.Contains(string(b), "secret-target") {
			t.Fatalf("%s marshals the child list: %s", name, b)
		}
	}
}
