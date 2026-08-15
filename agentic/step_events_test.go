// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

func TestStepTitle(t *testing.T) {
	cases := []struct {
		tool  string
		input string
		want  string
	}{
		{"bash", `{"command":"go test ./..."}`, "bash: go test ./..."},
		{"edit", `{"file_path":"internal/tui/model.go"}`, "edit: internal/tui/model.go"},
		{"list_tools", `{}`, "list_tools: listing tools"},
		{"todo", `{}`, "todo"},
	}
	for _, c := range cases {
		if got := stepTitle(c.tool, json.RawMessage(c.input)); got != c.want {
			t.Errorf("stepTitle(%s) = %q, want %q", c.tool, got, c.want)
		}
	}
	// Long labels are truncated.
	long := stepTitle("bash", json.RawMessage(`{"command":"`+strings.Repeat("x", 200)+`"}`))
	if len(long) > 100 {
		t.Errorf("title not truncated: %d chars", len(long))
	}
}

func TestSubAgentStepTitle(t *testing.T) {
	if got := subAgentStepTitle("audit the msg package\nwith details"); got != "sub-agent: audit the msg package" {
		t.Errorf("got %q", got)
	}
	if got := subAgentStepTitle("  "); got != "sub-agent" {
		t.Errorf("empty task: got %q", got)
	}
}

func TestSubAgentResultDigest(t *testing.T) {
	done := &msg.MsgOrchestratorDone{
		Success:      true,
		Summary:      "Changed 2 file(s): a.go, b.go.",
		FilesChanged: []string{"a.go", "b.go"},
	}
	d := subAgentResultDigest(done)
	if !strings.HasPrefix(d, "✓ done") || !strings.Contains(d, "2 file(s) changed") {
		t.Errorf("digest = %q", d)
	}
	fail := subAgentResultDigest(&msg.MsgOrchestratorDone{Success: false, Errors: []string{"boom"}})
	if !strings.HasPrefix(fail, "✗ failed") {
		t.Errorf("fail digest = %q", fail)
	}
	if got := subAgentResultDigest(nil); got == "" {
		t.Error("nil digest empty")
	}
}

func TestPausedStepTitle(t *testing.T) {
	if got := pausedStepTitle(msg.StoppedReasonCancelled); !strings.Contains(got, "interrupted") {
		t.Errorf("cancelled title = %q", got)
	}
	if got := pausedStepTitle(msg.StoppedReasonMaxIterations); !strings.Contains(got, "iteration") {
		t.Errorf("iteration title = %q", got)
	}
	if got := pausedStepTitle("other"); got != "paused" {
		t.Errorf("unknown reason title = %q", got)
	}
}
