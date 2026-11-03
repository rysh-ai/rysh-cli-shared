// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// Design 010 §3 S5: the host may supply per-turn recall (auto-recall). It is
// computed once per user turn from that turn's prompt, under a deadline, and
// appended to that turn's system prompt only: never to the transcript.

func TestRecallBlockIsAppendedPerTurn(t *testing.T) {
	var gotPrompt string
	var hadDeadline bool
	a := &LLMPromptExecutionActor{
		systemPrompt:     "BASE",
		envBlockProvider: func() string { return "ENV" },
		memoryState:      &msg.MemoryState{Entries: []msg.MemoryEntry{{Summary: "remember this"}}},
	}
	a.SetRecallProvider(func(ctx context.Context, prompt string) string {
		gotPrompt = prompt
		dl, ok := ctx.Deadline()
		hadDeadline = ok && time.Until(dl) <= recallTimeout
		return "# Recalled\nRYSH.md#Deploy: use the rollback script"
	})
	a.prepareRecall("how do I deploy?")
	if gotPrompt != "how do I deploy?" || !hadDeadline {
		t.Errorf("provider got prompt %q, deadline %v", gotPrompt, hadDeadline)
	}
	got := a.buildEffectiveSystemPrompt()
	iEnv, iMem, iRec := strings.Index(got, "ENV"), strings.Index(got, "remember this"), strings.Index(got, "# Recalled")
	if iRec < 0 || !(iEnv < iMem && iMem < iRec) {
		t.Errorf("want base, env, memory, then recall; got %q", got)
	}

	// The next turn replaces it: an empty recall leaves nothing behind.
	a.SetRecallProvider(func(context.Context, string) string { return "" })
	a.prepareRecall("unrelated")
	if strings.Contains(a.buildEffectiveSystemPrompt(), "Recalled") {
		t.Errorf("a previous turn's recall leaked into the next turn")
	}
}

func TestNoRecallProviderChangesNothing(t *testing.T) {
	a := &LLMPromptExecutionActor{systemPrompt: "BASE"}
	a.prepareRecall("anything")
	if got := a.buildEffectiveSystemPrompt(); got != "BASE" {
		t.Errorf("got %q, want BASE", got)
	}
}
