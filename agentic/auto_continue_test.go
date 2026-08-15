// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// TestAutoContinueDecision covers the auto-continue budget rules: resume only
// when armed, with resumes left, within the deadline, and paused specifically on
// a budget cap — never on a user cancel or a question, so an unbounded loop or a
// resume-through-a-question can't happen.
func TestAutoContinueDecision(t *testing.T) {
	future := time.Unix(1_000_000, 0)
	now := time.Unix(999_000, 0)    // before the deadline
	past := time.Unix(1_001_000, 0) // after the deadline
	noDeadline := time.Time{}

	cases := []struct {
		name          string
		armed         bool
		left          int
		deadline      time.Time
		now           time.Time
		stoppedReason string
		want          bool
	}{
		{"iter cap, budget left, no deadline", true, 3, noDeadline, now, msg.StoppedReasonMaxIterations, true},
		{"duration cap, budget left, before deadline", true, 1, future, now, msg.StoppedReasonMaxDuration, true},
		{"not armed", false, 3, noDeadline, now, msg.StoppedReasonMaxIterations, false},
		{"no resumes left", true, 0, noDeadline, now, msg.StoppedReasonMaxIterations, false},
		{"deadline passed", true, 3, future, past, msg.StoppedReasonMaxIterations, false},
		{"user cancel never auto-resumes", true, 3, noDeadline, now, msg.StoppedReasonCancelled, false},
		{"awaiting-info never auto-resumes", true, 3, noDeadline, now, msg.StoppedReasonAwaitingInfo, false},
		{"token cap never auto-resumes (context won't shrink)", true, 3, noDeadline, now, msg.StoppedReasonMaxTokens, false},
		{"unknown reason never auto-resumes", true, 3, noDeadline, now, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := autoContinueDecision(c.armed, c.left, c.deadline, c.stoppedReason, c.now); got != c.want {
				t.Errorf("autoContinueDecision(%v,%d,%v,%q) = %v, want %v",
					c.armed, c.left, c.deadline, c.stoppedReason, got, c.want)
			}
		})
	}
}

// TestShouldRunFinalizer covers when the graceful finalizer fires: only for an
// armed run that stopped on a budget cap, with a finalizer set, and not already
// finalizing — never on a plain prompt, a cancel, a question, or twice.
func TestShouldRunFinalizer(t *testing.T) {
	const fin = "wrap up gracefully"
	cases := []struct {
		name          string
		armed         bool
		stoppedReason string
		finalizer     string
		phase         bool
		want          bool
	}{
		{"iter cap, armed, finalizer set", true, msg.StoppedReasonMaxIterations, fin, false, true},
		{"duration cap, armed, finalizer set", true, msg.StoppedReasonMaxDuration, fin, false, true},
		{"token cap, armed, finalizer set", true, msg.StoppedReasonMaxTokens, fin, false, true},
		{"not armed (plain prompt hitting cap)", false, msg.StoppedReasonMaxIterations, fin, false, false},
		{"no finalizer configured", true, msg.StoppedReasonMaxIterations, "", false, false},
		{"already finalizing (no re-entry)", true, msg.StoppedReasonMaxIterations, fin, true, false},
		{"user cancel does not finalize", true, msg.StoppedReasonCancelled, fin, false, false},
		{"question does not finalize", true, msg.StoppedReasonAwaitingInfo, fin, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRunFinalizer(c.armed, c.stoppedReason, c.finalizer, c.phase); got != c.want {
				t.Errorf("shouldRunFinalizer(armed=%v,reason=%q,fin=%q,phase=%v) = %v, want %v",
					c.armed, c.stoppedReason, c.finalizer, c.phase, got, c.want)
			}
		})
	}
}

// TestGroundingModeForLeg verifies the finalizer/takeover leg runs with
// grounding OFF (it continues an already-grounded conversation and must spend
// its reserved budget on wrapping up), while ordinary legs keep the pane's mode.
func TestGroundingModeForLeg(t *testing.T) {
	if got := groundingModeForLeg("enforced", false); got != "enforced" {
		t.Errorf("ordinary leg should keep mode: got %q", got)
	}
	if got := groundingModeForLeg("enforced", true); got != "" {
		t.Errorf("finalizer leg should skip grounding: got %q", got)
	}
	if got := groundingModeForLeg("", false); got != "" {
		t.Errorf("grounding off stays off: got %q", got)
	}
}

// TestOutputDirNote verifies every synthetic leg restates the run's results
// directory (so it survives compaction of the original prompt), and that
// runs without one are unchanged.
func TestOutputDirNote(t *testing.T) {
	const dir = "/abs/guest-scout/results"
	if got := autoContinuePrompt(dir).Prompt; !strings.Contains(got, dir) || !strings.Contains(got, "Continue from where you stopped") {
		t.Errorf("continue nudge missing output dir: %q", got)
	}
	if got := autoContinuePrompt("").Prompt; strings.Contains(got, "Reminder") {
		t.Errorf("no output dir should add no reminder: %q", got)
	}
	if got := withOutputDirNote("wrap up", dir); !strings.Contains(got, dir) || !strings.HasPrefix(got, "wrap up") {
		t.Errorf("finalizer note wrong: %q", got)
	}
	if got := withOutputDirNote("wrap up", ""); got != "wrap up" {
		t.Errorf("empty dir should be a no-op: %q", got)
	}
}

// TestSetRunBudget_CarriesAndClearsFinalizer verifies the finalizer prompt is
// armed with the budget and cleared on disarm.
func TestSetRunBudget_CarriesAndClearsFinalizer(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50}
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, MaxTotalIterations: 100, FinalizerPrompt: "save what you have"})
	if a.finalizerPrompt != "save what you have" || a.finalizerPhase {
		t.Fatalf("finalizer not armed: prompt=%q phase=%v", a.finalizerPrompt, a.finalizerPhase)
	}
	a.disarmRunBudget()
	if a.finalizerPrompt != "" || a.finalizerPhase {
		t.Errorf("finalizer not cleared on disarm: prompt=%q phase=%v", a.finalizerPrompt, a.finalizerPhase)
	}
	// AutoContinue=false also clears any prior finalizer.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, FinalizerPrompt: "x"})
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: false})
	if a.finalizerPrompt != "" {
		t.Errorf("finalizer should be cleared when auto-continue disabled, got %q", a.finalizerPrompt)
	}
}

// TestSetRunBudget_StepIntervalAndTokens verifies step_interval drives the
// per-leg cap and leg count, and the working-context token cap is stored/cleared.
func TestSetRunBudget_StepIntervalAndTokens(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50}

	// step_interval 100 with a 300 total → 3 legs → 2 automatic resumes.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{
		AutoContinue: true, MaxTotalIterations: 300, StepInterval: 100, MaxContextTokens: 50000,
	})
	if a.runStepInterval != 100 {
		t.Errorf("runStepInterval = %d, want 100", a.runStepInterval)
	}
	if a.autoContinuesLeft != 2 {
		t.Errorf("autoContinuesLeft = %d, want 2 (300/100 legs)", a.autoContinuesLeft)
	}
	if a.runMaxContextTokens != 50000 {
		t.Errorf("runMaxContextTokens = %d, want 50000", a.runMaxContextTokens)
	}

	// Omitted step_interval falls back to the actor default (50).
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, MaxTotalIterations: 300})
	if a.runStepInterval != 50 {
		t.Errorf("fallback runStepInterval = %d, want 50", a.runStepInterval)
	}

	a.disarmRunBudget()
	if a.runStepInterval != 0 || a.runMaxContextTokens != 0 {
		t.Errorf("run overrides not cleared: step=%d tokens=%d", a.runStepInterval, a.runMaxContextTokens)
	}
}

// TestArmFinalizerBudget verifies the finalizer takes over with its RESERVED
// sub-budget: steps → leg allowance, duration → fresh deadline, token cap → the
// finalizer's context ceiling. Unset fields fall back to the built-in allowance.
func TestArmFinalizerBudget(t *testing.T) {
	// Reserved sub-budget: 30 steps, 2m, 50k-token context ceiling; step_interval 50.
	a := &LLMPromptExecutionActor{
		paneID: "p", maxIterations: 50, runStepInterval: 50,
		runFinalizerMaxIter: 30, runFinalizerMaxDur: 2 * time.Minute, runFinalizerMaxTokens: 50000,
		runTokensUsed: 123456, // the task's usage; must reset for the finalizer's fresh share
	}
	a.armFinalizerBudget()
	if a.runTokensUsed != 0 {
		t.Errorf("finalizer token accounting not reset: runTokensUsed=%d", a.runTokensUsed)
	}
	if a.autoContinuesLeft != 0 { // ceil(30/50)=1 leg → 0 resumes
		t.Errorf("autoContinuesLeft = %d, want 0", a.autoContinuesLeft)
	}
	if a.autoContinueMaxDur != 2*time.Minute || a.autoContinueDeadline.IsZero() {
		t.Errorf("finalizer deadline not set: dur=%s zero=%v", a.autoContinueMaxDur, a.autoContinueDeadline.IsZero())
	}
	if a.runMaxContextTokens != 50000 {
		t.Errorf("finalizer token cap = %d, want 50000 (full budget headroom)", a.runMaxContextTokens)
	}

	// A larger step allowance yields more finalizer legs.
	a2 := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50, runStepInterval: 50, runFinalizerMaxIter: 120}
	a2.armFinalizerBudget()
	if a2.autoContinuesLeft != 2 { // ceil(120/50)=3 legs → 2 resumes
		t.Errorf("autoContinuesLeft = %d, want 2", a2.autoContinuesLeft)
	}

	// Unset sub-budget → built-in fallback.
	a3 := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50, runStepInterval: 50}
	a3.armFinalizerBudget()
	if a3.autoContinuesLeft != finalizerMaxResumes || a3.autoContinueMaxDur != finalizerDeadline {
		t.Errorf("fallback not applied: left=%d dur=%s", a3.autoContinuesLeft, a3.autoContinueMaxDur)
	}
	if a3.runMaxContextTokens != 0 {
		t.Errorf("no finalizer token cap should leave it 0 (lifted), got %d", a3.runMaxContextTokens)
	}
}

// TestSetRunBudget_AutoApprove verifies step.auto_approve is captured (armed and
// even without auto-continue) and cleared at run end.
func TestSetRunBudget_AutoApprove(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50}

	// Armed run with auto-approve on.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, AutoApprove: true, MaxTotalIterations: 100})
	if !a.runAutoApprove {
		t.Error("runAutoApprove should be true when armed with AutoApprove")
	}
	// Run end clears it (so later normal prompts still require approval).
	a.disarmRunBudget()
	if a.runAutoApprove {
		t.Error("runAutoApprove should be cleared on disarm")
	}

	// Auto-approve applies even when auto-continue is off.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: false, AutoApprove: true})
	if !a.runAutoApprove {
		t.Error("runAutoApprove should be set even without auto-continue")
	}
	// Explicitly off.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, AutoApprove: false, MaxTotalIterations: 100})
	if a.runAutoApprove {
		t.Error("runAutoApprove should be false when AutoApprove is false")
	}
}

// TestHandleSetRunBudget covers arming (leg allowance derived from the total
// iteration budget and the per-leg cap) and clearing.
func TestHandleSetRunBudget(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50}

	// 300 total / 50 per-leg → 6 legs → 5 automatic resumes; 20m deadline armed.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, MaxTotalIterations: 300, MaxDurationMs: (20 * time.Minute).Milliseconds()})
	if !a.autoContinueArmed {
		t.Fatal("budget should be armed")
	}
	if a.autoContinuesLeft != 5 {
		t.Errorf("autoContinuesLeft = %d, want 5", a.autoContinuesLeft)
	}
	if a.autoContinueDeadline.IsZero() {
		t.Error("deadline should be set when a duration is given")
	}

	// A total below one leg still yields a single leg (0 resumes), no underflow.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: true, MaxTotalIterations: 10})
	if a.autoContinuesLeft != 0 {
		t.Errorf("autoContinuesLeft = %d, want 0 for sub-leg total", a.autoContinuesLeft)
	}
	if !a.autoContinueDeadline.IsZero() {
		t.Error("deadline should be zero when no duration is given")
	}

	// AutoContinue=false clears everything.
	a.handleSetRunBudget(&msg.MsgSetRunBudget{AutoContinue: false})
	if a.autoContinueArmed || a.autoContinuesLeft != 0 || !a.autoContinueDeadline.IsZero() {
		t.Errorf("budget not cleared: armed=%v left=%d deadline=%v", a.autoContinueArmed, a.autoContinuesLeft, a.autoContinueDeadline)
	}
}

// TestBuildRunStatusReply verifies the ##auto <kind> runs accounting snapshot:
// arming a budget stamps runArmedAt and the reply carries the live consumption
// (tokens, remaining resumes, in-flight flag); after disarm the reply reads
// Armed=false + InFlight=false — the "run ended, prune me" signal.
func TestBuildRunStatusReply(t *testing.T) {
	a := &LLMPromptExecutionActor{paneID: "p", maxIterations: 50}
	a.handleSetRunBudget(&msg.MsgSetRunBudget{
		AutoContinue: true, MaxTotalIterations: 300, StepInterval: 100,
		MaxContextTokens: 50000, MaxDurationMs: int64(time.Hour / time.Millisecond),
	})
	a.runTokensUsed = 12345
	a.activeOrchID = "orch-1"

	r := a.buildRunStatusReply()
	if !r.Armed || !r.InFlight || r.Finalizer {
		t.Fatalf("armed run misreported: %+v", r)
	}
	if r.TokensUsed != 12345 || r.MaxContextTokens != 50000 {
		t.Errorf("token accounting misreported: used=%d cap=%d", r.TokensUsed, r.MaxContextTokens)
	}
	if r.StepInterval != 100 || r.ContinuesLeft != 2 {
		t.Errorf("leg accounting misreported: interval=%d left=%d", r.StepInterval, r.ContinuesLeft)
	}
	if r.ArmedAtMs == 0 || r.DeadlineMs == 0 {
		t.Errorf("timestamps not stamped: armedAt=%d deadline=%d", r.ArmedAtMs, r.DeadlineMs)
	}

	// Run ends: orchestrator gone + budget disarmed → prune signal.
	a.activeOrchID = ""
	a.disarmRunBudget()
	r = a.buildRunStatusReply()
	if r.Armed || r.InFlight || r.TokensUsed != 0 || r.ArmedAtMs != 0 {
		t.Errorf("ended run should read idle: %+v", r)
	}
}
