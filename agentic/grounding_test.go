// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

func TestParseGroundingReport(t *testing.T) {
	p, err := ParseGroundingReport(json.RawMessage(
		`{"understood":true,"relevant_files":["a.go","b.go"],"evidence":"found the handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Understood || len(p.RelevantFiles) != 2 || p.Evidence == "" {
		t.Errorf("parse lost fields: %+v", p)
	}

	q, err := ParseGroundingReport(json.RawMessage(
		`{"understood":false,"question":"Where does the billing webhook live?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.Understood || q.Question == "" {
		t.Errorf("question parse lost fields: %+v", q)
	}

	if _, err := ParseGroundingReport(json.RawMessage(`{not json`)); err == nil {
		t.Error("invalid JSON should error")
	}

	// Empty payload parses to zero value (understood=false).
	z, err := ParseGroundingReport(nil)
	if err != nil || z.Understood {
		t.Errorf("empty payload: %+v err=%v", z, err)
	}
}

func TestGroundingAllowedTools(t *testing.T) {
	allowed := []string{"glob", "grep", "symbol_search", "file_read", "bash", "ask_user", GroundingToolName}
	for _, name := range allowed {
		if !groundingAllowed(name) {
			t.Errorf("%s should be allowed while grounding", name)
		}
	}
	blocked := []string{"edit", "file_write", "git_commit", "pane_send", "memory_edit", "bash_background", SubAgentToolName, "some_mcp_tool"}
	for _, name := range blocked {
		if groundingAllowed(name) {
			t.Errorf("%s should be BLOCKED while grounding", name)
		}
	}
}

func TestGroundingBlockedMessage(t *testing.T) {
	m := groundingBlockedMessage("edit")
	for _, want := range []string{`"edit"`, "grounding", "grounding_report", "read-only"} {
		if !strings.Contains(m, want) {
			t.Errorf("blocked message missing %q: %s", want, m)
		}
	}
}

func TestGroundedDigest(t *testing.T) {
	if got := groundedDigest(&GroundingReportParams{}); !strings.Contains(got, "no codebase context") {
		t.Errorf("empty digest = %q", got)
	}
	if got := groundedDigest(&GroundingReportParams{RelevantFiles: []string{"x.go"}}); got != "grounded on x.go" {
		t.Errorf("single-file digest = %q", got)
	}
	multi := groundedDigest(&GroundingReportParams{RelevantFiles: []string{"x.go", "y.go", "z.go"}})
	if !strings.Contains(multi, "3 files") || !strings.Contains(multi, "x.go") {
		t.Errorf("multi-file digest = %q", multi)
	}
}

func TestGroundingPrompts(t *testing.T) {
	// The enforced prompt must extend the advisory protocol with the gate
	// contract, so hosts overriding the advisory text keep both in sync.
	if !strings.Contains(DefaultGroundingPromptEnforced, "Grounding protocol") {
		t.Error("enforced prompt should contain the protocol")
	}
	for _, want := range []string{"grounding_report", "understood: true", "understood: false"} {
		if !strings.Contains(DefaultGroundingPromptEnforced, want) {
			t.Errorf("enforced prompt missing %q", want)
		}
	}
	for _, want := range []string{"glob", "grep", "symbol_search", "DO NOT GUESS"} {
		if !strings.Contains(DefaultGroundingPromptAdvisory, want) {
			t.Errorf("advisory prompt missing %q", want)
		}
	}
}

// groundingCall builds a grounding_report tool-call request for tests.
func groundingCall(params string) provider.ToolCallRequest {
	return provider.ToolCallRequest{ID: "t1", Name: GroundingToolName, Input: json.RawMessage(params)}
}

// TestHandleGroundingReport_GateAndQuestion exercises the intercepted
// pseudo-tool directly on bare orchestrators (emit paths are nil-publisher
// safe, so only state is asserted).
func TestHandleGroundingReport_GateAndQuestion(t *testing.T) {
	// understood=true opens the gate.
	o := &OrchestratorActor{groundingMode: GroundingEnforced, grounding: true}
	out := o.handleGroundingReport(groundingCall(`{"understood":true,"relevant_files":["main.go"]}`))
	if out.Error != "" {
		t.Fatalf("unexpected error: %s", out.Error)
	}
	if o.grounding {
		t.Error("gate should be open after understood=true")
	}
	if !strings.Contains(out.Content, "unlocked") {
		t.Errorf("ack content = %q", out.Content)
	}

	// understood=false with a question schedules the awaiting-info pause.
	o2 := &OrchestratorActor{groundingMode: GroundingEnforced, grounding: true}
	out2 := o2.handleGroundingReport(groundingCall(`{"understood":false,"question":"Where is the billing code?"}`))
	if out2.Error != "" {
		t.Fatalf("unexpected error: %s", out2.Error)
	}
	if o2.pendingQuestion != "Where is the billing code?" {
		t.Errorf("pendingQuestion = %q", o2.pendingQuestion)
	}
	if !o2.grounding {
		t.Error("gate stays closed while awaiting the answer")
	}

	// understood=true in prompt/off mode (no gate) still acks cleanly.
	o3 := &OrchestratorActor{groundingMode: GroundingPrompt}
	out3 := o3.handleGroundingReport(groundingCall(`{"understood":true}`))
	if out3.Error != "" || !strings.Contains(out3.Content, "Proceed") {
		t.Errorf("prompt-mode ack: %+v", out3)
	}

	// understood=false without question/missing_info is a validation error.
	o4 := &OrchestratorActor{}
	out4 := o4.handleGroundingReport(groundingCall(`{"understood":false}`))
	if out4.Error == "" {
		t.Error("missing question should be a validation error")
	}
}

// TestGroundingGate_BlocksMutatingTools verifies executeTool's gate check
// rejects mutating tools while grounding without reaching the registry.
func TestGroundingGate_BlocksMutatingTools(t *testing.T) {
	o := &OrchestratorActor{grounding: true}
	out := o.executeTool(nil, provider.ToolCallRequest{
		ID: "t2", Name: "edit", Input: json.RawMessage(`{"file_path":"a.go"}`),
	})
	if out.Error == "" || out.ErrorKind != "permission_denied" {
		t.Fatalf("expected permission_denied while grounding, got %+v", out)
	}
	if !strings.Contains(out.Error, "grounding_report") {
		t.Errorf("error should teach the unlock path: %s", out.Error)
	}
}

func TestValidGroundingMode(t *testing.T) {
	for _, ok := range []string{GroundingOff, GroundingPrompt, GroundingEnforced} {
		if !ValidGroundingMode(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "on", "ENFORCED", "strict"} {
		if ValidGroundingMode(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestLastGroundingReport(t *testing.T) {
	turns := []provider.ConversationTurn{
		{Role: "user", Content: "fix it"},
		{Role: "assistant", TimestampMs: 10, ToolCalls: []provider.ToolCallRequest{
			{ID: "g1", Name: GroundingToolName, Input: json.RawMessage(`{"understood":false,"question":"where is it?"}`)},
		}},
		{Role: "tool", ToolCallID: "g1", Content: "Question surfaced..."},
		{Role: "user", Content: "it lives in internal/billing"},
		{Role: "assistant", TimestampMs: 20, ToolCalls: []provider.ToolCallRequest{
			{ID: "g2", Name: GroundingToolName, Input: json.RawMessage(
				`{"understood":true,"relevant_files":["internal/billing/charge.go"],"evidence":"found the idempotency bug"}`)},
		}},
		{Role: "tool", ToolCallID: "g2", Content: "Grounding accepted..."},
		{Role: "assistant", Content: "Done."},
	}

	r := LastGroundingReport(turns)
	if r == nil {
		t.Fatal("expected a report")
	}
	if !r.Understood || len(r.RelevantFiles) != 1 || r.RelevantFiles[0] != "internal/billing/charge.go" {
		t.Errorf("latest report not returned: %+v", r)
	}
	if r.TimestampMs != 20 {
		t.Errorf("timestamp should come from the assistant turn: %d", r.TimestampMs)
	}

	// No grounded run → nil.
	if LastGroundingReport([]provider.ConversationTurn{{Role: "user", Content: "hi"}}) != nil {
		t.Error("expected nil for ungrounded session")
	}
	if LastGroundingReport(nil) != nil {
		t.Error("expected nil for empty session")
	}
}

// TestGroundingAllowedCall_BrowserActions verifies the param-aware gate:
// observe actions pass while grounding, mutating actions are blocked.
func TestGroundingAllowedCall_BrowserActions(t *testing.T) {
	for _, action := range []string{"get_text", "get_html", "get_elements", "get_value", "get_tabs", "screenshot", "wait"} {
		if !groundingAllowedCall("browser_action", json.RawMessage(`{"action":"`+action+`"}`)) {
			t.Errorf("observe action %q should be allowed while grounding", action)
		}
	}
	for _, action := range []string{"click", "type", "navigate", "select", "check", "hover", "press_key", "drag_drop", "execute_js", "new_tab", "close_tab", "switch_tab", "scroll", "back", "forward", "reload"} {
		if groundingAllowedCall("browser_action", json.RawMessage(`{"action":"`+action+`"}`)) {
			t.Errorf("mutating action %q should be BLOCKED while grounding", action)
		}
	}
	// Malformed params fail closed.
	if groundingAllowedCall("browser_action", json.RawMessage(`{not json`)) {
		t.Error("malformed browser_action params should be blocked")
	}
	// Non-browser tools keep the name-based decision.
	if !groundingAllowedCall("grep", nil) || groundingAllowedCall("edit", nil) {
		t.Error("name-based gate regressed for non-browser tools")
	}
}
