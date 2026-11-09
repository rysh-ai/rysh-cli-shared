// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"testing"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// QA-36: a delivery to a busy agent (the courier, `rysh family serve`) must
// wait for the run in flight, never cancel it. A Queue prompt waits in order
// behind the active run; only an ordinary prompt keeps last-prompt-wins.

func TestHandlePrompt_QueuedPromptNeverCancelsTheActiveRun(t *testing.T) {
	cancelled := false
	a := &LLMPromptExecutionActor{
		paneID:     "pane-1",
		activeOrch: &actor.PID{},
		cancelFn:   func() { cancelled = true },
	}
	first := &msg.MsgAgenticPrompt{RequestID: "c1", Prompt: "courier delivery", Queue: true}
	second := &msg.MsgAgenticPrompt{RequestID: "c2", Prompt: "another delivery", Queue: true}
	a.handlePrompt(nil, first)
	a.handlePrompt(nil, second)

	if cancelled || a.cancelFn == nil {
		t.Fatal("a Queue prompt cancelled the run in flight")
	}
	if a.pendingPrompt != nil {
		t.Fatalf("a Queue prompt took the last-prompt-wins slot: %+v", a.pendingPrompt)
	}
	if len(a.queuedPrompts) != 2 || a.queuedPrompts[0] != first || a.queuedPrompts[1] != second {
		t.Fatalf("queued = %+v, want both in arrival order", a.queuedPrompts)
	}
}

// When the run ends, a superseding (ordinary) prompt still goes first, then
// the queue in order.
func TestNextPrompt_SupersedingFirstThenQueueInOrder(t *testing.T) {
	q1 := &msg.MsgAgenticPrompt{RequestID: "q1", Queue: true}
	q2 := &msg.MsgAgenticPrompt{RequestID: "q2", Queue: true}
	user := &msg.MsgAgenticPrompt{RequestID: "u"}
	a := &LLMPromptExecutionActor{pendingPrompt: user, queuedPrompts: []*msg.MsgAgenticPrompt{q1, q2}}
	for _, want := range []*msg.MsgAgenticPrompt{user, q1, q2, nil} {
		if got := a.nextPrompt(); got != want {
			t.Fatalf("nextPrompt = %+v, want %+v", got, want)
		}
	}
}

// The queue is bounded: past the cap a Queue prompt is refused, never
// silently dropped and never allowed to cancel the run.
func TestHandlePrompt_QueueIsBounded(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "pane-1", activeOrch: &actor.PID{}, cancelFn: func() {}}
	for i := 0; i < maxQueuedPrompts+3; i++ {
		a.handlePrompt(nil, &msg.MsgAgenticPrompt{Queue: true})
	}
	if len(a.queuedPrompts) != maxQueuedPrompts {
		t.Fatalf("queued %d, want the cap %d", len(a.queuedPrompts), maxQueuedPrompts)
	}
	if a.cancelFn == nil {
		t.Fatal("an overflowing Queue prompt cancelled the run")
	}
}

// The run's first status names the prompt it runs, so a caller that sent a
// RequestID can tell its own run from one queued ahead of it.
func TestPlanningStatusCarriesTheRequestID(t *testing.T) {
	st := runStatus("orch-1", "req-7", "planning", 0, 10)
	if st.RequestID != "req-7" || st.OrchestratorID != "orch-1" || st.Phase != "planning" {
		t.Fatalf("status = %+v", st)
	}
}
