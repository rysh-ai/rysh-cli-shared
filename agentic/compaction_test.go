package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

func TestSafeCompactionCutIndex_AdvancesPastToolTurns(t *testing.T) {
	o := &OrchestratorActor{
		conversation: []provider.ConversationTurn{
			{Role: "user"}, // 0
			{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{ID: "a"}}}, // 1
			{Role: "tool", ToolCallID: "a"},                                       // 2
			{Role: "assistant"},                                                   // 3
			{Role: "user"},                                                        // 4
		},
	}

	// Desired index 2 lands on a tool-result turn; the cut must advance to 3
	// (an assistant turn) so the retained head is not an orphaned tool_result.
	got := o.safeCompactionCutIndex(2)
	if got != 3 {
		t.Fatalf("expected cut advanced to 3, got %d", got)
	}
	if o.conversation[got].Role == "tool" {
		t.Fatalf("retained head must not be a tool turn")
	}
}

func TestSafeCompactionCutIndex_NonPositive(t *testing.T) {
	o := &OrchestratorActor{conversation: []provider.ConversationTurn{{Role: "user"}}}
	if got := o.safeCompactionCutIndex(0); got != 0 {
		t.Fatalf("expected 0 for non-positive desired, got %d", got)
	}
}

func TestSafeCompactionCutIndex_AllTrailingTools(t *testing.T) {
	// If advancing past tool turns consumes everything, bail out with 0.
	o := &OrchestratorActor{
		conversation: []provider.ConversationTurn{
			{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{ID: "a"}}},
			{Role: "tool", ToolCallID: "a"},
			{Role: "tool", ToolCallID: "a"},
		},
	}
	if got := o.safeCompactionCutIndex(1); got != 0 {
		t.Fatalf("expected 0 when no safe boundary remains, got %d", got)
	}
}

// TestCompactionSummaryContent covers the original-task pinning: the first
// compaction embeds the run's first user prompt verbatim; successive
// compactions carry the pinned block forward; oversized briefs are truncated;
// non-task heads yield summary-only content.
func TestCompactionSummaryContent(t *testing.T) {
	task := "Scout Instagram for tango guests. Save results to /abs/path/guest-scout/results as <topic>-<date>.md."

	// First compaction: head is the original user prompt → pinned verbatim.
	first := compactionSummaryContent(provider.ConversationTurn{Role: "user", Content: task}, "browsed 40 profiles")
	if !strings.HasPrefix(first, pinnedTaskHeader) || !strings.Contains(first, task) || !strings.Contains(first, "browsed 40 profiles") {
		t.Fatalf("first compaction should pin the task:\n%s", first)
	}

	// Second compaction: head is the previous compaction turn → the pinned
	// block survives, the old summary is replaced by the new one.
	second := compactionSummaryContent(provider.ConversationTurn{Role: "user", Origin: "compaction", Content: first}, "verified 5 candidates")
	if !strings.Contains(second, task) || !strings.Contains(second, "verified 5 candidates") {
		t.Fatalf("second compaction lost the pinned task:\n%s", second)
	}
	if strings.Contains(second, "browsed 40 profiles") {
		t.Errorf("second compaction should not carry the previous summary:\n%s", second)
	}

	// A previous compaction turn WITHOUT a pinned block (legacy) → summary only.
	legacy := compactionSummaryContent(provider.ConversationTurn{Role: "user", Origin: "compaction", Content: compactionSummaryHeader + "\n\nold"}, "new summary")
	if strings.Contains(legacy, pinnedTaskHeader) || !strings.Contains(legacy, "new summary") {
		t.Errorf("legacy compaction head handled wrong:\n%s", legacy)
	}

	// Oversized brief is truncated at the cap.
	huge := strings.Repeat("x", pinnedTaskMaxChars+500)
	trunc := compactionSummaryContent(provider.ConversationTurn{Role: "user", Content: huge}, "s")
	if !strings.Contains(trunc, "[...original task truncated...]") || len(trunc) > pinnedTaskMaxChars+300 {
		t.Errorf("oversized task not truncated: len=%d", len(trunc))
	}

	// Non-user / empty heads → summary only.
	if got := compactionSummaryContent(provider.ConversationTurn{Role: "assistant", Content: "hi"}, "s"); strings.Contains(got, pinnedTaskHeader) {
		t.Errorf("assistant head should not be pinned: %s", got)
	}
	if got := compactionSummaryContent(provider.ConversationTurn{Role: "user", Content: "  "}, "s"); strings.Contains(got, pinnedTaskHeader) {
		t.Errorf("empty head should not be pinned: %s", got)
	}
}

func TestMechanicalDigest(t *testing.T) {
	turns := []provider.ConversationTurn{
		{Role: "user"},
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{Name: "bash"}, {Name: "grep"}}},
		{Role: "user"},
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{Name: "bash"}}},
	}
	got := mechanicalDigest(turns)
	if !strings.Contains(got, "2 user message") {
		t.Errorf("expected user count in digest, got %q", got)
	}
	if !strings.Contains(got, "3 tool call") {
		t.Errorf("expected tool call count in digest, got %q", got)
	}
	if !strings.Contains(got, "bash") || !strings.Contains(got, "grep") {
		t.Errorf("expected tool names in digest, got %q", got)
	}
}

func TestRenderTranscript(t *testing.T) {
	turns := []provider.ConversationTurn{
		{Role: "user", Content: "fix the bug"},
		{Role: "assistant", Content: "looking", ToolCalls: []provider.ToolCallRequest{{Name: "grep", Input: []byte(`{"pattern":"x"}`)}}},
		{Role: "tool", Content: "match found"},
	}
	got := renderTranscript(turns)
	for _, want := range []string{"USER: fix the bug", "ASSISTANT: looking", "called tool grep", "TOOL RESULT: match found"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q in:\n%s", want, got)
		}
	}
}
