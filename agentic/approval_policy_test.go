package agentic

import (
	"encoding/json"
	"testing"
)

// TestDecideApproval_PolicyOverrides is the safety proof for design 013: a
// policy GATE must force approval even when the run is headless auto-approve
// (a humanoid running unattended) or the call was previously "approve all like
// this" — restrictive wins. And a policy AUTO must skip approval the classifier
// would otherwise require. Without the policy consult, always_gate/bash.deny in
// a written policy would silently do nothing under auto-approve.
func TestDecideApproval_PolicyOverrides(t *testing.T) {
	t.Cleanup(func() { SetApprovalPolicy(nil) })

	// A policy that gates "bash" and auto-approves "file_read".
	SetApprovalPolicy(func(tool string, _ json.RawMessage) (PolicyDecision, string) {
		switch tool {
		case "bash":
			return PolicyGate, "bash.deny[0]"
		case "file_read":
			return PolicyAutoApprove, "approval.auto_approve[0]"
		}
		return PolicyDefault, ""
	})

	// Headless auto-approve on, classifier says no approval needed — a GATE rule
	// must still force approval, and cite the rule.
	o := &OrchestratorActor{autoApproved: map[string]bool{}, autoApproveAll: true}
	if need, rule := o.decideApproval("bash", json.RawMessage(`{"command":"rm -rf /"}`), false); !need || rule != "bash.deny[0]" {
		t.Fatalf("gate under autoApproveAll = (need=%v rule=%q), want (true, bash.deny[0])", need, rule)
	}

	// The per-session "approve all like this" registry is likewise overridden.
	o2 := &OrchestratorActor{autoApproved: map[string]bool{"bash:x": true}, autoApproveAll: false}
	if need, _ := o2.decideApproval("bash", json.RawMessage(`{"command":"x"}`), true); !need {
		t.Fatal("gate did not override the session auto-approve registry")
	}

	// AUTO rule skips approval the classifier would otherwise require.
	if need, rule := o.decideApproval("file_read", json.RawMessage(`{}`), true); need || rule != "approval.auto_approve[0]" {
		t.Fatalf("auto-approve = (need=%v rule=%q), want (false, approval.auto_approve[0])", need, rule)
	}

	// No matching rule → fall through to the classifier verdict, no rule cited.
	// Use a non-headless orchestrator so the classifier's "true" is what stands.
	o3 := &OrchestratorActor{autoApproved: map[string]bool{}, autoApproveAll: false}
	if need, rule := o3.decideApproval("web_fetch", json.RawMessage(`{}`), true); !need || rule != "" {
		t.Fatalf("default path = (need=%v rule=%q), want (true, \"\")", need, rule)
	}
}

// TestDecideApproval_NoPolicy confirms the classifier and session flags behave
// exactly as before when no policy is installed (no regression).
func TestDecideApproval_NoPolicy(t *testing.T) {
	SetApprovalPolicy(nil)
	o := &OrchestratorActor{autoApproved: map[string]bool{}, autoApproveAll: false}
	if need, rule := o.decideApproval("bash", json.RawMessage(`{"command":"x"}`), true); !need || rule != "" {
		t.Fatalf("classifier-requires with no policy = (%v, %q), want (true, \"\")", need, rule)
	}
	o.autoApproveAll = true
	if need, _ := o.decideApproval("bash", json.RawMessage(`{"command":"x"}`), true); need {
		t.Fatal("autoApproveAll should skip approval when no policy gates it")
	}
}
