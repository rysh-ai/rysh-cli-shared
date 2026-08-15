// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// TestSessionMemory_AppendCategorizes verifies user turns and generic appends
// carry categories + timestamps in the session JSON.
func TestSessionMemory_AppendCategorizes(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	s.AppendUserTurn("hello", nil)
	s.Append(provider.ConversationTurn{Role: "assistant", Content: "hi"})
	s.Append(provider.ConversationTurn{Role: "tool", Content: "ok", ToolCallID: "t1"})

	turns := s.Turns()
	if turns[0].Category != provider.TurnCategoryUser {
		t.Errorf("user category = %q", turns[0].Category)
	}
	if turns[1].Category != provider.TurnCategoryAI {
		t.Errorf("assistant category = %q", turns[1].Category)
	}
	if turns[2].Category != provider.TurnCategoryTool {
		t.Errorf("tool category = %q", turns[2].Category)
	}
	for i, turn := range turns {
		if turn.TimestampMs == 0 {
			t.Errorf("turn %d missing timestamp", i)
		}
	}
}

// TestSessionMemory_CategoryInJSON verifies the category survives the KV
// serialization round-trip alongside the legacy un-tagged fields.
func TestSessionMemory_CategoryInJSON(t *testing.T) {
	turn := provider.ConversationTurn{
		Role:     "tool",
		Content:  "result",
		Category: provider.TurnCategorySubAgent,
		Origin:   "orch-123",
		Summary:  "sub-agent: audit — ✓ done",
	}
	data, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"category":"subagent"`, `"origin":"orch-123"`, `"summary":`, `"Role":"tool"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("serialized turn missing %s: %s", want, data)
		}
	}

	var back provider.ConversationTurn
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Category != provider.TurnCategorySubAgent || back.Origin != "orch-123" {
		t.Errorf("round-trip lost metadata: %+v", back)
	}
}

// TestSessionMemory_PauseCheckpoint verifies the pause/resume checkpoint
// lifecycle.
func TestSessionMemory_PauseCheckpoint(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	if paused, _ := s.Paused(); paused {
		t.Fatal("fresh session should not be paused")
	}
	s.Pause("cancelled")
	paused, reason := s.Paused()
	if !paused || reason != "cancelled" {
		t.Fatalf("pause not recorded: %v %q", paused, reason)
	}
	s.ClearPause()
	if paused, _ := s.Paused(); paused {
		t.Fatal("pause should be cleared")
	}
}

// TestCloseDanglingToolCalls verifies an interrupted run's unanswered
// tool_use blocks get synthetic error results so the transcript is resumable.
func TestCloseDanglingToolCalls(t *testing.T) {
	turns := []provider.ConversationTurn{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{
			{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"go test"}`)},
			{ID: "t2", Name: "edit", Input: json.RawMessage(`{"file_path":"a.go"}`)},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "ok"}, // t1 answered, t2 dangling
	}
	healed := CloseDanglingToolCalls(turns, "interrupted")
	if len(healed) != 4 {
		t.Fatalf("want 4 turns, got %d", len(healed))
	}
	last := healed[3]
	if last.Role != "tool" || last.ToolCallID != "t2" || !last.IsError {
		t.Errorf("synthetic result malformed: %+v", last)
	}
	if last.Category != provider.TurnCategoryTool || last.Origin != "edit" {
		t.Errorf("synthetic result not categorized: %+v", last)
	}
	if !strings.Contains(last.Content, "kind=cancelled") {
		t.Errorf("synthetic result should carry cancelled error kind: %q", last.Content)
	}

	// Idempotent: all answered → unchanged.
	again := CloseDanglingToolCalls(healed, "interrupted")
	if len(again) != len(healed) {
		t.Errorf("healing a healed transcript changed it: %d → %d", len(healed), len(again))
	}

	// No trailing assistant tool_use → unchanged.
	plain := []provider.ConversationTurn{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	if got := CloseDanglingToolCalls(plain, "x"); len(got) != 2 {
		t.Errorf("plain transcript changed: %d", len(got))
	}
}

// TestSessionMemory_ReplaceBackfillsCategories verifies pre-categorisation
// transcripts (e.g. restored from old KV data) get categories derived from
// roles.
func TestSessionMemory_ReplaceBackfillsCategories(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	s.Replace([]provider.ConversationTurn{
		{Role: "user", Content: "old prompt"},
		{Role: "assistant", Content: "old answer"},
	})
	turns := s.Turns()
	if turns[0].Category != provider.TurnCategoryUser || turns[1].Category != provider.TurnCategoryAI {
		t.Errorf("categories not backfilled: %+v", turns)
	}
}

// TestSessionMemoryState_WrapperRoundTrip verifies the persisted wrapper
// format including the pause checkpoint.
func TestSessionMemoryState_WrapperRoundTrip(t *testing.T) {
	state := SessionMemoryState{
		Version:      sessionMemoryVersion,
		Turns:        []provider.ConversationTurn{{Role: "user", Content: "x", Category: "user"}},
		Paused:       true,
		PausedReason: "max_iterations",
		UpdatedMs:    123,
	}
	data, _ := json.Marshal(state)
	var back SessionMemoryState
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Paused || back.PausedReason != "max_iterations" || len(back.Turns) != 1 {
		t.Errorf("wrapper round-trip lost state: %+v", back)
	}
}

// TestSessionMemory_GroundingOverride verifies the ##grounding override is
// carried in the persisted wrapper and restored even when no turns exist yet.
func TestSessionMemory_GroundingOverride(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	if s.GroundingOverride() != "" {
		t.Fatal("fresh session should have no override")
	}
	s.SetGroundingOverride("prompt")
	if s.GroundingOverride() != "prompt" {
		t.Fatal("override not recorded")
	}
	s.SetGroundingOverride("")
	if s.GroundingOverride() != "" {
		t.Fatal("override not cleared")
	}

	// Wrapper round-trip with an override but ZERO turns (fresh pane): the
	// override must survive serialization, and old wrappers without the field
	// must unmarshal with no override.
	state := SessionMemoryState{Version: sessionMemoryVersion, GroundingOverride: "enforced"}
	data, _ := json.Marshal(state)
	if !strings.Contains(string(data), `"grounding_override":"enforced"`) {
		t.Errorf("override missing from wrapper JSON: %s", data)
	}
	var back SessionMemoryState
	if err := json.Unmarshal(data, &back); err != nil || back.GroundingOverride != "enforced" {
		t.Errorf("override lost in round-trip: %+v err=%v", back, err)
	}
	var legacy SessionMemoryState
	if err := json.Unmarshal([]byte(`{"version":1,"turns":[]}`), &legacy); err != nil || legacy.GroundingOverride != "" {
		t.Errorf("pre-override wrapper should restore with no override: %+v err=%v", legacy, err)
	}
}
