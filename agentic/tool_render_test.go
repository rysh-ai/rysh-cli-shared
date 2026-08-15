// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestToolCallHeader verifies the "⏺ name(label)" header rendering, including
// the no-label fallback to "⏺ name".
func TestToolCallHeader(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{
			name:  "bash command",
			tool:  "bash",
			input: `{"command":"go test ./..."}`,
			want:  `⏺ bash(go test ./...)`,
		},
		{
			name:  "file_read path",
			tool:  "file_read",
			input: `{"file_path":"internal/tui/model.go"}`,
			want:  `⏺ file_read(internal/tui/model.go)`,
		},
		{
			name:  "grep with path",
			tool:  "grep",
			input: `{"pattern":"emitOutput","path":"agentic"}`,
			want:  `⏺ grep(emitOutput in agentic)`,
		},
		{
			name:  "no label falls back to bare name",
			tool:  "process_list",
			input: `{}`,
			want:  `⏺ process_list`,
		},
		{
			name:  "unparseable input falls back to bare name",
			tool:  "bash",
			input: `not json`,
			want:  `⏺ bash`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCallHeader(tc.tool, json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("toolCallHeader(%q, %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

// TestToolResultSummary covers the success line-count variants and the error
// variants (with and without an exit code).
func TestToolResultSummary(t *testing.T) {
	cases := []struct {
		name   string
		output *tools.ToolOutput
		want   string
	}{
		{"empty content", &tools.ToolOutput{Content: ""}, "✓"},
		{"trailing newline only", &tools.ToolOutput{Content: "\n"}, "✓"},
		{"single line", &tools.ToolOutput{Content: "just one"}, "✓ 1 line"},
		{"single line trailing nl", &tools.ToolOutput{Content: "just one\n"}, "✓ 1 line"},
		{"three lines", &tools.ToolOutput{Content: "a\nb\nc"}, "✓ 3 lines"},
		{"three lines trailing nl", &tools.ToolOutput{Content: "a\nb\nc\n"}, "✓ 3 lines"},
		{"error no exit code", &tools.ToolOutput{Error: "file not found"}, "✗ file not found"},
		{"error first line only", &tools.ToolOutput{Error: "boom\nstack trace\nmore"}, "✗ boom"},
		{"error with exit code", &tools.ToolOutput{Error: "cannot find package", ExitCode: 1}, "✗ exit 1: cannot find package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolResultSummary(tc.output); got != tc.want {
				t.Errorf("toolResultSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestToolResultBranch verifies the indented branch wrapper that sits beneath a
// tool-call header.
func TestToolResultBranch(t *testing.T) {
	if got, want := toolResultBranch("✓ 3 lines"), "  ⎿  ✓ 3 lines\n"; got != want {
		t.Errorf("toolResultBranch = %q, want %q", got, want)
	}
}

// TestFirstLine verifies single-line extraction and trimming.
func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "hello"},
		{"  hello  ", "hello"},
		{"first\nsecond", "first"},
		{"\n\nfirst\nsecond", "first"},
		{"  first line  \nrest", "first line"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
