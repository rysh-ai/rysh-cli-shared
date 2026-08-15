// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"sync/atomic"
)

// Approval policy-as-code hook (design 013 §1).
//
// The orchestrator decides, per tool call, whether to auto-run or gate for
// human approval. Policy-as-code adds two permissive rule classes
// (approval.auto_approve, bash.allow) and two restrictive ones
// (approval.always_gate, bash.deny) that adjust that decision, and every gate
// decision cites the matched rule ID.
//
// The matching logic lives in rysh-cli's policy package (which loads
// .rysh/policy.yaml), but the decision is consumed here in the orchestrator —
// and rysh-shared cannot import rysh-cli. So the evaluator is injected as a
// process-global func, mirroring the fail-closed gate (rysh-cli's
// policy.SetBlocked) and the proxy endpoint global. Process-wide is deliberate:
// in farm mode the last-installed policy applies to every orchestrator, and the
// restrictive-wins resolution keeps that erring toward safety.

// PolicyDecision is the outcome of consulting the approval policy for one call.
type PolicyDecision int

const (
	// PolicyDefault — no rule matched; fall back to the tool's own classifier.
	PolicyDefault PolicyDecision = iota
	// PolicyAutoApprove — a permissive rule matched; skip approval.
	PolicyAutoApprove
	// PolicyGate — a restrictive rule matched; force approval. This OVERRIDES
	// the per-session "approve all like this" registry and headless
	// auto-approve, so an unattended humanoid still gates a policy-gated call.
	PolicyGate
)

// ApprovalPolicyFunc evaluates one tool call (name + raw JSON input) against
// policy-as-code and returns a decision plus the matched rule ID (e.g.
// "bash.deny[1]") for traceability. Returns (PolicyDefault, "") when no rule
// matches.
type ApprovalPolicyFunc func(toolName string, input json.RawMessage) (PolicyDecision, string)

var approvalPolicy atomic.Pointer[ApprovalPolicyFunc]

// SetApprovalPolicy installs the approval policy (nil clears it). Called by
// rysh-cli's WorkspaceActor on policy load and on every ##policy reload.
func SetApprovalPolicy(f ApprovalPolicyFunc) {
	if f == nil {
		approvalPolicy.Store(nil)
		return
	}
	approvalPolicy.Store(&f)
}

// evalApprovalPolicy consults the installed policy, or (PolicyDefault, "") when
// none is installed.
func evalApprovalPolicy(toolName string, input json.RawMessage) (PolicyDecision, string) {
	p := approvalPolicy.Load()
	if p == nil {
		return PolicyDefault, ""
	}
	return (*p)(toolName, input)
}
