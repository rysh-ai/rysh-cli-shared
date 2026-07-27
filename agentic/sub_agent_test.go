package agentic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// stubTool is a minimal tools.ToolExecutor used solely so the registry has
// something concrete to clone/filter. None of its methods run during the
// tests; only Spec() is touched when the registry enumerates Names().
type stubTool struct{ name string }

func (s *stubTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: s.name, Description: s.name}
}
func (s *stubTool) RequiresApproval(json.RawMessage) bool { return false }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (*tools.ToolOutput, error) {
	return &tools.ToolOutput{}, nil
}

// TestParseSubAgentParams covers required/optional/whitespace handling and
// the missing-task error path.
func TestParseSubAgentParams(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantTsk string
	}{
		{"missing task", `{}`, true, ""},
		{"blank task whitespace", `{"task":"   "}`, true, ""},
		{"minimal", `{"task":"refactor"}`, false, "refactor"},
		{"trim task", `{"task":"  do thing  "}`, false, "do thing"},
		{"with all fields", `{"task":"x","context":"c","system_prompt":"sp","allowed_tools":["bash","grep"]}`, false, "x"},
		{"invalid json", `{not-json`, true, ""},
		{"empty raw", ``, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseSubAgentParams(json.RawMessage(c.input))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got params=%+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Task != c.wantTsk {
				t.Errorf("task = %q, want %q", p.Task, c.wantTsk)
			}
		})
	}
}

// TestParseSubAgentParams_AllFieldsRoundTrip ensures non-required fields are
// preserved through parsing.
func TestParseSubAgentParams_AllFieldsRoundTrip(t *testing.T) {
	raw := `{"task":"x","context":"see bar","system_prompt":"sp","allowed_tools":["bash","grep"]}`
	p, err := ParseSubAgentParams(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Context != "see bar" {
		t.Errorf("context = %q", p.Context)
	}
	if p.SystemPrompt != "sp" {
		t.Errorf("system_prompt = %q", p.SystemPrompt)
	}
	if got := strings.Join(p.AllowedTools, ","); got != "bash,grep" {
		t.Errorf("allowed_tools = %v", p.AllowedTools)
	}
}

// TestBuildSubAgentRegistry_AllowedFilter checks that an explicit allowedTools
// list narrows the registry, and that an empty list inherits the full parent.
func TestBuildSubAgentRegistry_AllowedFilter(t *testing.T) {
	parent := tools.NewToolRegistry()
	parent.Register("bash", &stubTool{name: "bash"})
	parent.Register("grep", &stubTool{name: "grep"})
	parent.Register("file_read", &stubTool{name: "file_read"})

	t.Run("nil allowed = full clone", func(t *testing.T) {
		child := BuildSubAgentRegistry(parent, nil)
		got := child.Names()
		want := []string{"bash", "file_read", "grep"}
		if !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})
	t.Run("empty allowed = full clone", func(t *testing.T) {
		child := BuildSubAgentRegistry(parent, []string{})
		if got := len(child.Names()); got != 3 {
			t.Errorf("expected 3 tools, got %v", child.Names())
		}
	})
	t.Run("explicit subset", func(t *testing.T) {
		child := BuildSubAgentRegistry(parent, []string{"bash", "grep"})
		got := child.Names()
		want := []string{"bash", "grep"}
		if !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})
	t.Run("unknown tools silently skipped", func(t *testing.T) {
		child := BuildSubAgentRegistry(parent, []string{"bash", "does_not_exist", "  ", "grep"})
		got := child.Names()
		want := []string{"bash", "grep"}
		if !equalStrings(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})
	t.Run("nil parent yields empty", func(t *testing.T) {
		child := BuildSubAgentRegistry(nil, []string{"bash"})
		if got := len(child.Names()); got != 0 {
			t.Errorf("nil parent should yield empty registry, got %d tools", got)
		}
	})
}

// TestBuildSubAgentSeedConversation verifies the user turn carries the task
// alone when context is empty, and the labelled context appendix when provided.
func TestBuildSubAgentSeedConversation(t *testing.T) {
	t.Run("task only", func(t *testing.T) {
		conv := BuildSubAgentSeedConversation(&SubAgentParams{Task: "refactor foo"})
		if len(conv) != 1 || conv[0].Role != "user" {
			t.Fatalf("unexpected conv: %+v", conv)
		}
		if conv[0].Content != "refactor foo" {
			t.Errorf("content = %q", conv[0].Content)
		}
	})
	t.Run("task and context", func(t *testing.T) {
		conv := BuildSubAgentSeedConversation(&SubAgentParams{Task: "refactor foo", Context: "see file bar.go"})
		body := conv[0].Content
		if !strings.Contains(body, "refactor foo") || !strings.Contains(body, "see file bar.go") {
			t.Errorf("missing task or context in: %q", body)
		}
		if !strings.Contains(body, "Additional context:") {
			t.Errorf("missing context label in: %q", body)
		}
	})
	t.Run("context whitespace ignored", func(t *testing.T) {
		conv := BuildSubAgentSeedConversation(&SubAgentParams{Task: "x", Context: "   "})
		if strings.Contains(conv[0].Content, "Additional context:") {
			t.Errorf("blank context should not emit label, got %q", conv[0].Content)
		}
	})
}

// TestFormatSubAgentSummary verifies the rendered summary surfaces success,
// failure, the final assistant text, and the file/error tallies.
func TestFormatSubAgentSummary(t *testing.T) {
	t.Run("nil done", func(t *testing.T) {
		got := FormatSubAgentSummary(nil)
		if !strings.Contains(got, "no result") {
			t.Errorf("expected 'no result' summary, got %q", got)
		}
	})
	t.Run("success with final answer", func(t *testing.T) {
		done := &msg.MsgOrchestratorDone{
			Success: true,
			Summary: "Renamed three functions.",
			Conversation: []provider.ConversationTurn{
				{Role: "user", Content: "do work"},
				{Role: "assistant", Content: "I renamed foo to bar."},
			},
			FilesChanged: []string{"a.go", "b.go"},
		}
		got := FormatSubAgentSummary(done)
		for _, want := range []string{"completed successfully", "Renamed three functions.", "I renamed foo to bar.", "a.go, b.go"} {
			if !strings.Contains(got, want) {
				t.Errorf("summary missing %q in:\n%s", want, got)
			}
		}
	})
	t.Run("failure with errors", func(t *testing.T) {
		done := &msg.MsgOrchestratorDone{
			Success: false,
			Errors:  []string{"reached max iterations"},
		}
		got := FormatSubAgentSummary(done)
		if !strings.Contains(got, "reported failure") {
			t.Errorf("expected failure label, got %q", got)
		}
		if !strings.Contains(got, "reached max iterations") {
			t.Errorf("expected error text, got %q", got)
		}
	})
}

// TestLastAssistantText prefers the most recent non-empty assistant turn,
// skipping tool turns and empty assistant turns.
func TestLastAssistantText(t *testing.T) {
	conv := []provider.ConversationTurn{
		{Role: "user", Content: "ask"},
		{Role: "assistant", Content: "earlier"},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: ""},
		{Role: "assistant", Content: "final answer"},
		{Role: "tool", Content: "more"},
	}
	if got := lastAssistantText(conv); got != "final answer" {
		t.Errorf("got %q, want %q", got, "final answer")
	}
	if got := lastAssistantText(nil); got != "" {
		t.Errorf("nil conv should yield empty, got %q", got)
	}
}

// TestSubAgentConstants pins the depth/iteration constants so accidental
// changes are caught.
func TestSubAgentConstants(t *testing.T) {
	if MaxSubAgentDepth < 1 {
		t.Errorf("MaxSubAgentDepth should be >= 1, got %d", MaxSubAgentDepth)
	}
	if DefaultSubAgentMaxIterations <= 0 {
		t.Errorf("DefaultSubAgentMaxIterations should be positive, got %d", DefaultSubAgentMaxIterations)
	}
	if SubAgentToolName != "sub_agent" {
		t.Errorf("tool name should be %q, got %q", "sub_agent", SubAgentToolName)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
