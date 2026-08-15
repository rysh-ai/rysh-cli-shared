// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewMemoryEntry(t *testing.T) {
	turnIDs := []string{"turn-1", "turn-2", "turn-3"}
	entry := NewMemoryEntry(ConvAI, "User discussed file editing", turnIDs)

	if entry.ID == "" {
		t.Error("expected non-empty ID")
	}
	if entry.Mode != ConvAI {
		t.Errorf("Mode: got %q, want %q", entry.Mode, ConvAI)
	}
	if entry.Summary != "User discussed file editing" {
		t.Errorf("Summary: got %q", entry.Summary)
	}
	if entry.TurnCount != 3 {
		t.Errorf("TurnCount: got %d, want 3", entry.TurnCount)
	}
	if entry.CreatedAtMs == 0 {
		t.Error("CreatedAtMs should be non-zero")
	}
}

func TestMemoryEntry_JSONRoundTrip(t *testing.T) {
	entry := NewMemoryEntry(ConvSlack, "Discussed deployment strategy", []string{"t1", "t2"})

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MemoryEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != entry.ID {
		t.Errorf("ID: got %q, want %q", decoded.ID, entry.ID)
	}
	if decoded.Mode != ConvSlack {
		t.Errorf("Mode: got %q, want %q", decoded.Mode, ConvSlack)
	}
	if decoded.TurnCount != 2 {
		t.Errorf("TurnCount: got %d, want 2", decoded.TurnCount)
	}
}

func TestMemoryState_JSONRoundTrip(t *testing.T) {
	state := &MemoryState{
		PaneID: "pane-123",
		Mode:   ConvAI,
		Entries: []MemoryEntry{
			*NewMemoryEntry(ConvAI, "First batch summary", []string{"t1", "t2"}),
			*NewMemoryEntry(ConvAI, "Second batch summary", []string{"t3", "t4", "t5"}),
		},
		TotalSummarizedTurns: 5,
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MemoryState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.PaneID != "pane-123" {
		t.Errorf("PaneID: got %q", decoded.PaneID)
	}
	if len(decoded.Entries) != 2 {
		t.Errorf("Entries: got %d, want 2", len(decoded.Entries))
	}
	if decoded.TotalSummarizedTurns != 5 {
		t.Errorf("TotalSummarizedTurns: got %d, want 5", decoded.TotalSummarizedTurns)
	}
}

func TestFormatMemoryForPrompt_Empty(t *testing.T) {
	// Nil state
	if result := FormatMemoryForPrompt(nil); result != "" {
		t.Errorf("expected empty for nil state, got %q", result)
	}

	// Empty entries
	state := &MemoryState{Entries: []MemoryEntry{}}
	if result := FormatMemoryForPrompt(state); result != "" {
		t.Errorf("expected empty for no entries, got %q", result)
	}
}

func TestFormatMemoryForPrompt_WithEntries(t *testing.T) {
	state := &MemoryState{
		PaneID: "p1",
		Mode:   ConvAI,
		Entries: []MemoryEntry{
			{
				ID:          "mem-1",
				Summary:     "Worked on auth module",
				TurnCount:   20,
				CreatedAtMs: 1700000000000,
			},
			{
				ID:          "mem-2",
				Summary:     "Refactored database layer",
				TurnCount:   15,
				CreatedAtMs: 1700000000000,
			},
		},
	}

	result := FormatMemoryForPrompt(state)

	if !strings.Contains(result, "## Conversation Memory") {
		t.Error("expected '## Conversation Memory' header")
	}
	if !strings.Contains(result, "### Memory 1") {
		t.Error("expected '### Memory 1' entry")
	}
	if !strings.Contains(result, "### Memory 2") {
		t.Error("expected '### Memory 2' entry")
	}
	if !strings.Contains(result, "Worked on auth module") {
		t.Error("expected first summary content")
	}
	if !strings.Contains(result, "Refactored database layer") {
		t.Error("expected second summary content")
	}
	if !strings.Contains(result, "covers 20 turns") {
		t.Error("expected turn count in Memory 1")
	}
	if !strings.Contains(result, "## Recent Conversation") {
		t.Error("expected '## Recent Conversation' footer")
	}
}

func TestMemorySummarizationPrompt(t *testing.T) {
	turns := []ConversationMessage{
		{TurnID: "t1", TurnType: TurnQuestion, Content: "What is the status?"},
		{TurnID: "t1", TurnType: TurnAnswer, Content: "All tests pass."},
	}

	prompt := MemorySummarizationPrompt(ConvAI, turns)

	if !strings.Contains(prompt, "ai mode") {
		t.Error("expected mode reference in prompt")
	}
	if !strings.Contains(prompt, "What is the status?") {
		t.Error("expected turn content in prompt")
	}
	if !strings.Contains(prompt, "Key decisions made") {
		t.Error("expected summarization instructions")
	}
}

func TestMsgMemoryAppend_CodecRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()

	entry := NewMemoryEntry(ConvAI, "Test summary", []string{"t1"})
	m := &MsgMemoryAppend{PaneID: "pane-1", Entry: *entry}

	tag := reg.TagOf(m)
	if tag != TagMemoryAppend {
		t.Errorf("TagOf: got %q, want %q", tag, TagMemoryAppend)
	}

	payload, _ := json.Marshal(m)
	decoded, err := reg.Decode(TagMemoryAppend, payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	result, ok := decoded.(*MsgMemoryAppend)
	if !ok {
		t.Fatalf("expected *MsgMemoryAppend, got %T", decoded)
	}
	if result.PaneID != "pane-1" {
		t.Errorf("PaneID: got %q", result.PaneID)
	}
	if result.Entry.Summary != "Test summary" {
		t.Errorf("Entry.Summary: got %q", result.Entry.Summary)
	}
}

func TestMsgMemoryStateUpdate_CodecRoundTrip(t *testing.T) {
	reg := DefaultCodecRegistry()

	state := &MemoryState{
		PaneID:  "p1",
		Mode:    ConvAI,
		Entries: []MemoryEntry{*NewMemoryEntry(ConvAI, "summary", []string{"t1"})},
	}
	m := &MsgMemoryStateUpdate{State: state}

	tag := reg.TagOf(m)
	if tag != TagMemoryStateUpdate {
		t.Errorf("TagOf: got %q, want %q", tag, TagMemoryStateUpdate)
	}

	payload, _ := json.Marshal(m)
	decoded, err := reg.Decode(TagMemoryStateUpdate, payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	result, ok := decoded.(*MsgMemoryStateUpdate)
	if !ok {
		t.Fatalf("expected *MsgMemoryStateUpdate, got %T", decoded)
	}
	if result.State.PaneID != "p1" {
		t.Errorf("State.PaneID: got %q", result.State.PaneID)
	}
}
