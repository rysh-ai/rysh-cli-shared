// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// TestToolCallHeaderLine verifies a tool-call header is forced onto its own
// line: when the stream is mid-line (preamble text without a trailing newline)
// a leading newline is prepended; when already at line start it is not.
func TestToolCallHeaderLine(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		atLineStart bool
		want        string
	}{
		{"mid-line gets leading newline", "⏺ bash(ls)", false, "\n⏺ bash(ls)\n"},
		{"at line start no leading newline", "⏺ bash(ls)", true, "⏺ bash(ls)\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolCallHeaderLine(c.line, c.atLineStart); got != c.want {
				t.Errorf("toolCallHeaderLine(%q,%v) = %q, want %q", c.line, c.atLineStart, got, c.want)
			}
		})
	}
}

// TestBuildEffectiveSystemPrompt covers per-turn env-block injection and memory
// appending. The actor's prompt assembly reads only struct fields, so a bare
// literal exercises it without NATS.
func TestBuildEffectiveSystemPrompt(t *testing.T) {
	t.Run("no provider, no memory", func(t *testing.T) {
		a := &LLMPromptExecutionActor{systemPrompt: "BASE"}
		if got := a.buildEffectiveSystemPrompt(); got != "BASE" {
			t.Errorf("got %q, want BASE", got)
		}
	})

	t.Run("env block appended", func(t *testing.T) {
		a := &LLMPromptExecutionActor{
			systemPrompt:     "BASE",
			envBlockProvider: func() string { return "Working directory: /tmp/proj" },
		}
		got := a.buildEffectiveSystemPrompt()
		if !strings.Contains(got, "BASE") || !strings.Contains(got, "Working directory: /tmp/proj") {
			t.Errorf("env block not appended: %q", got)
		}
		if strings.Index(got, "BASE") > strings.Index(got, "Working directory") {
			t.Errorf("base must precede env block: %q", got)
		}
	})

	t.Run("empty env block is skipped", func(t *testing.T) {
		a := &LLMPromptExecutionActor{
			systemPrompt:     "BASE",
			envBlockProvider: func() string { return "" },
		}
		if got := a.buildEffectiveSystemPrompt(); got != "BASE" {
			t.Errorf("got %q, want BASE (empty env block must not add separators)", got)
		}
	})

	t.Run("env block then memory", func(t *testing.T) {
		a := &LLMPromptExecutionActor{
			systemPrompt:     "BASE",
			envBlockProvider: func() string { return "ENV" },
			memoryState:      &msg.MemoryState{Entries: []msg.MemoryEntry{{Summary: "remember this"}}},
		}
		got := a.buildEffectiveSystemPrompt()
		if !strings.Contains(got, "BASE") || !strings.Contains(got, "ENV") || !strings.Contains(got, "remember this") {
			t.Errorf("expected base+env+memory, got %q", got)
		}
		if !(strings.Index(got, "BASE") < strings.Index(got, "ENV") && strings.Index(got, "ENV") < strings.Index(got, "remember this")) {
			t.Errorf("expected order base < env < memory, got %q", got)
		}
	})
}
