// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// F-47: choice_selected is a legal ApprovalDecision (the web door accepts it,
// ApprovalOverlay sends it), but approval gates never offer choices — the only
// request that carries any is ask_user's question, which reads the answer
// itself and never reaches a gate. A choice arriving at a gate used to fall
// into the "no response" branch: the pane printed [No response - skipping],
// the web echo printed [approved], and the model was told nobody answered.
// The answer must be named as what it is, and the tool must not run.
func TestGateRefusesChoiceSelectedByName(t *testing.T) {
	o := &OrchestratorActor{}
	out := o.unmatchedApprovalDecision(&msg.MsgApprovalResponse{
		RequestID: "req-1",
		Decision:  msg.DecisionChoiceSelected,
		ChoiceIdx: 1,
	}, "No approval received.")

	if out == nil || out.Error == "" {
		t.Fatalf("a choice at a gate must not run the tool; got %+v", out)
	}
	if out.Error == "no approval" || strings.Contains(out.Content, "No approval received") {
		t.Fatalf("choice_selected reported as an unanswered gate (F-47): %+v", out)
	}
	if !strings.Contains(out.Content, "choice_selected") || !strings.Contains(out.Content, "option 2") {
		t.Fatalf("model is not told the gate got a choice (option 2): %q", out.Content)
	}
}

// The default branch keeps meaning "no response" for everything else — e.g. an
// empty decision.
func TestGateUnknownDecisionStillNoApproval(t *testing.T) {
	o := &OrchestratorActor{}
	out := o.unmatchedApprovalDecision(&msg.MsgApprovalResponse{RequestID: "req-2"}, "No approval received.")
	if out.Error != "no approval" || out.Content != "No approval received." {
		t.Fatalf("got %+v", out)
	}
}
