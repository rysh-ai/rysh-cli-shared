// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// TestMergeDoneIntoSession_MergesSupersededRun is the direct regression for the
// "Ask Rysh forgets the previous turn" bug. A user sends turn 1, and before its
// orchestrator finishes sends turn 2. Turn 2 supersedes turn 1: the running
// orchestrator is cancelled and delivers its Done with Interrupted=true, but it
// STILL carries the full conversation it built (the assistant's "noted" reply
// and the tool exchange that captured the passphrase). The old code dropped
// that conversation on the stale-id guard, so turn 2 ran against a transcript
// with no memory of the passphrase. The fix merges it — verified here.
func TestMergeDoneIntoSession_MergesSupersededRun(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	// The session as it stood when turn 1's orchestrator was spawned: just the
	// user's request. (Turn 2's user turn has not been appended yet — that
	// happens in startPrompt AFTER this merge.)
	s.AppendUserTurn("Remember this passphrase: BANANA-42. Reply with just: noted.", nil)

	// The finished (superseded) run's full conversation: the request, the
	// assistant's tool exchange, and its final answer.
	done := &msg.MsgOrchestratorDone{
		OrchestratorID: "orch-A",
		Interrupted:    true, // cancelled by the superseding prompt
		StoppedReason:  "superseded",
		Conversation: []provider.ConversationTurn{
			{Role: "user", Content: "Remember this passphrase: BANANA-42. Reply with just: noted.", Category: provider.TurnCategoryUser},
			{Role: "assistant", Content: "", ToolCalls: []provider.ToolCallRequest{{ID: "t1", Name: "todo"}}, Category: provider.TurnCategoryAI},
			{Role: "tool", ToolCallID: "t1", Content: "stored: passphrase BANANA-42", Category: provider.TurnCategoryTool, Origin: "todo"},
			{Role: "assistant", Content: "noted", Category: provider.TurnCategoryAI},
		},
	}

	// activeOrchID matches the finished run → authoritative → must merge.
	if ok := mergeDoneIntoSession(s, "orch-A", done); !ok {
		t.Fatal("matching Done reported stale; the run's work would be dropped")
	}

	turns := s.Turns()
	if len(turns) < len(done.Conversation) {
		t.Fatalf("superseded run's conversation was dropped: session has %d turns, want >= %d", len(turns), len(done.Conversation))
	}
	// The follow-up prompt must be able to see the passphrase: it survives as
	// both the tool result and the assistant's "noted" acknowledgement.
	if !containsContent(turns, "BANANA-42") {
		t.Errorf("passphrase lost from merged session — follow-up turn would not remember it: %+v", turns)
	}
	if last := turns[len(turns)-1]; last.Role != "assistant" || last.Content != "noted" {
		t.Errorf("merged session should end with the run's final answer, got %+v", last)
	}
}

// TestMergeDoneIntoSession_IgnoresStaleId verifies a genuinely stale Done (a
// duplicate/rogue completion whose id doesn't match the active run) is ignored
// and leaves the session untouched — we only drop UNRECOGNIZED runs, never the
// matching one.
func TestMergeDoneIntoSession_IgnoresStaleId(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	s.AppendUserTurn("hello", nil)
	before := len(s.Turns())

	done := &msg.MsgOrchestratorDone{
		OrchestratorID: "orch-OLD",
		Conversation:   []provider.ConversationTurn{{Role: "user", Content: "stale"}, {Role: "assistant", Content: "stale answer"}},
	}
	if ok := mergeDoneIntoSession(s, "orch-CURRENT", done); ok {
		t.Fatal("stale Done reported authoritative")
	}
	if got := len(s.Turns()); got != before {
		t.Errorf("stale Done mutated the session: %d → %d turns", before, got)
	}
}

// TestMergeDoneIntoSession_DropsAbandonedRun verifies the ##hop-fork path:
// handleSessionMemoryReplace clears activeOrchID to "" after installing the
// forked memory, so a late Done from the stopped orchestrator (any real id)
// mismatches and is dropped — it must NOT clobber the freshly forked turns.
func TestMergeDoneIntoSession_DropsAbandonedRun(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	s.Replace([]provider.ConversationTurn{{Role: "user", Content: "forked memory"}})
	before := len(s.Turns())

	done := &msg.MsgOrchestratorDone{
		OrchestratorID: "orch-abandoned",
		Conversation:   []provider.ConversationTurn{{Role: "user", Content: "stale run"}, {Role: "assistant", Content: "stale answer"}},
	}
	// activeOrchID == "" means the actor is not expecting any Done (run abandoned).
	if ok := mergeDoneIntoSession(s, "", done); ok {
		t.Fatal("abandoned-run Done reported authoritative; forked memory would be clobbered")
	}
	if got := len(s.Turns()); got != before {
		t.Errorf("abandoned-run Done mutated forked memory: %d → %d turns", before, got)
	}
}

// TestMergeDoneIntoSession_AppendsSummaryFallback verifies that when a Done
// carries no conversation (in-process field lost, e.g. a serialized Done) but a
// text Summary, the summary is appended so the session isn't left empty.
func TestMergeDoneIntoSession_AppendsSummaryFallback(t *testing.T) {
	s := NewSessionMemory("pane-1", nil)
	s.AppendUserTurn("do it", nil)
	done := &msg.MsgOrchestratorDone{OrchestratorID: "orch-A", Summary: "did it"}
	if ok := mergeDoneIntoSession(s, "orch-A", done); !ok {
		t.Fatal("matching Done reported stale")
	}
	turns := s.Turns()
	last := turns[len(turns)-1]
	if last.Role != "assistant" || last.Content != "did it" || last.Category != provider.TurnCategoryAI {
		t.Errorf("summary not appended as AI turn: %+v", last)
	}
}

// TestHandlePrompt_QueuesBehindActiveRun verifies orchestrators are serialized:
// a prompt arriving while a run is active does NOT spawn a second orchestrator
// (which would race the first and drop its work). Instead it is parked in
// pendingPrompt and the running orchestrator is cancelled via cancelFn — its
// Done later merges its work and starts the queued prompt. If this branch were
// wrong, control would fall through to startPrompt(ctx=nil, ...) and panic,
// which also fails the test.
func TestHandlePrompt_QueuesBehindActiveRun(t *testing.T) {
	cancelled := false
	a := &LLMPromptExecutionActor{
		paneID:     "pane-1",
		activeOrch: &actor.PID{}, // a run is in flight
		cancelFn:   func() { cancelled = true },
	}
	p := &msg.MsgAgenticPrompt{Prompt: "follow-up while busy"}

	// ctx is unused on the queue path; nil is safe (and if the code wrongly
	// falls through to startPrompt it panics → test fails).
	a.handlePrompt(nil, p)

	if a.pendingPrompt != p {
		t.Fatalf("superseding prompt was not queued: pendingPrompt=%+v", a.pendingPrompt)
	}
	if !cancelled {
		t.Error("running orchestrator was not cancelled")
	}
	if a.cancelFn != nil {
		t.Error("cancelFn should be cleared after cancelling")
	}
}

// TestHandlePrompt_LastPromptWins verifies a newer superseding prompt overwrites
// an older queued one (last-prompt-wins), so only the most recent follow-up
// runs after the in-flight orchestrator finishes.
func TestHandlePrompt_LastPromptWins(t *testing.T) {
	a := &LLMPromptExecutionActor{
		paneID:     "pane-1",
		activeOrch: &actor.PID{},
		cancelFn:   func() {},
	}
	first := &msg.MsgAgenticPrompt{Prompt: "first follow-up"}
	second := &msg.MsgAgenticPrompt{Prompt: "second follow-up"}
	a.handlePrompt(nil, first)
	a.handlePrompt(nil, second)
	if a.pendingPrompt != second {
		t.Errorf("expected last prompt to win, got %+v", a.pendingPrompt)
	}
}

// containsContent reports whether any turn's Content contains sub.
func containsContent(turns []provider.ConversationTurn, sub string) bool {
	for _, tn := range turns {
		if strings.Contains(tn.Content, sub) {
			return true
		}
	}
	return false
}
