// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"testing"
)

// TestPermissionPolicy_NilOrEmpty: no policy / empty rules yields PermAsk.
func TestPermissionPolicy_NilOrEmpty(t *testing.T) {
	var nilPol *PermissionPolicy
	if got := nilPol.Decide("bash", nil); got != PermAsk {
		t.Errorf("nil policy: got %v", got)
	}
	if got := NewPermissionPolicy(nil).Decide("bash", nil); got != PermAsk {
		t.Errorf("empty policy: got %v", got)
	}
}

// TestPermissionPolicy_FirstMatchWins documents the rule precedence: the
// first matching rule wins, so order in the slice matters.
func TestPermissionPolicy_FirstMatchWins(t *testing.T) {
	p := NewPermissionPolicy([]PermissionRule{
		{Tool: "file_read", Match: "**/.env*", Decision: PermDeny},
		{Tool: "file_read", Decision: PermAllow},
	})
	// Specific deny matches first.
	if got := p.Decide("file_read", json.RawMessage(`{"file_path": "/x/.env"}`)); got != PermDeny {
		t.Errorf("specific deny should win: %v", got)
	}
	// Falls through to the broad allow.
	if got := p.Decide("file_read", json.RawMessage(`{"file_path": "main.go"}`)); got != PermAllow {
		t.Errorf("broad allow should win: %v", got)
	}
}

// TestPermissionPolicy_ToolWildcard: empty Tool matches any tool name.
func TestPermissionPolicy_ToolWildcard(t *testing.T) {
	p := NewPermissionPolicy([]PermissionRule{
		{Tool: "", Match: "", Decision: PermDeny}, // deny everything
	})
	if got := p.Decide("bash", nil); got != PermDeny {
		t.Errorf("wildcard deny: %v", got)
	}
	if got := p.Decide("file_read", nil); got != PermDeny {
		t.Errorf("wildcard deny on file_read: %v", got)
	}
}

// TestPermissionPolicy_NoMatch: no rule matches → PermAsk.
func TestPermissionPolicy_NoMatch(t *testing.T) {
	p := NewPermissionPolicy([]PermissionRule{
		{Tool: "bash", Match: "ls *", Decision: PermAllow},
	})
	if got := p.Decide("bash", json.RawMessage(`{"command": "rm -rf /"}`)); got != PermAsk {
		t.Errorf("no match → ask, got %v", got)
	}
	if got := p.Decide("file_read", nil); got != PermAsk {
		t.Errorf("no match → ask, got %v", got)
	}
}

// TestPermissionPath_FieldSelection: bash → command, web_fetch → url,
// web_search/grep → pattern, default → file_path.
func TestPermissionPath_FieldSelection(t *testing.T) {
	cases := []struct {
		tool   string
		input  string
		expect string
	}{
		{"bash", `{"command": "ls -la"}`, "ls -la"},
		{"bash_background", `{"command": "tail -f log"}`, "tail -f log"},
		{"web_fetch", `{"url": "https://example.com"}`, "https://example.com"},
		{"web_search", `{"pattern": "hello"}`, "hello"},
		{"grep", `{"pattern": "TODO"}`, "TODO"},
		{"file_read", `{"file_path": "main.go"}`, "main.go"},
		{"file_edit", `{"file_path": "x.go"}`, "x.go"},
		{"unknown", `{"file_path": "y"}`, "y"},
		{"file_read", `{}`, ""},
		{"file_read", ``, ""},
		{"file_read", `not json`, ""},
	}
	for _, c := range cases {
		got := permissionPath(c.tool, json.RawMessage(c.input))
		if got != c.expect {
			t.Errorf("%s(%s) = %q, want %q", c.tool, c.input, got, c.expect)
		}
	}
}

// TestMatchGlob_StarBehaviour: standard filepath.Match + the prefix-and-
// space convenience.
func TestMatchGlob_StarBehaviour(t *testing.T) {
	cases := []struct {
		pattern, value string
		match          bool
	}{
		{"", "anything", true}, // empty pattern always matches
		{"x", "x", true},       // literal match
		{"x", "y", false},      // literal mismatch
		{"*.go", "main.go", true},
		{"*.go", "x.py", false},
		{"ls *", "ls", true},
		{"ls *", "ls -la", true},
		{"ls *", "lsof", false}, // requires the space boundary
		{"git status *", "git status", true},
		{"git status *", "git status --short", true},
		{"git status *", "git statusx", false},
		{"prefix*", "prefixanything", true},
		{"prefix*", "other", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.value); got != c.match {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.value, got, c.match)
		}
	}
}

// TestMatchGlob_Doublestar: ** crosses path separators.
func TestMatchGlob_Doublestar(t *testing.T) {
	cases := []struct {
		pattern, value string
		match          bool
	}{
		{"**/.env*", "/x/y/.env", true},
		{"**/.env*", "/x/y/.env.local", true},
		{"**/.env*", "/x/y/main.go", false},
		{"src/**", "src/a/b/c.go", true},
		{"src/**", "lib/x.go", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.value); got != c.match {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.value, got, c.match)
		}
	}
}

// TestPermissionPolicy_TypicalConfig is a higher-level scenario: an opinionated
// policy that mirrors what a careful operator might write.
func TestPermissionPolicy_TypicalConfig(t *testing.T) {
	p := NewPermissionPolicy([]PermissionRule{
		// Never write to secrets.
		{Tool: "file_write", Match: "**/.env*", Decision: PermDeny},
		{Tool: "file_edit", Match: "**/.env*", Decision: PermDeny},
		// Auto-allow read-only ops.
		{Tool: "file_read", Decision: PermAllow},
		{Tool: "grep", Decision: PermAllow},
		{Tool: "glob", Decision: PermAllow},
		{Tool: "ls", Decision: PermAllow},
		{Tool: "tree", Decision: PermAllow},
		// Allow safe bash subcommands.
		{Tool: "bash", Match: "ls *", Decision: PermAllow},
		{Tool: "bash", Match: "cat *", Decision: PermAllow},
		{Tool: "bash", Match: "git status*", Decision: PermAllow},
	})

	cases := []struct {
		tool   string
		input  string
		expect PermDecision
	}{
		{"file_write", `{"file_path": ".env"}`, PermDeny},
		{"file_edit", `{"file_path": "config/.env.local"}`, PermDeny},
		{"file_read", `{"file_path": "main.go"}`, PermAllow},
		{"grep", `{"pattern": "TODO"}`, PermAllow},
		{"bash", `{"command": "ls"}`, PermAllow},
		{"bash", `{"command": "ls -la"}`, PermAllow},
		{"bash", `{"command": "rm -rf /"}`, PermAsk},
		{"file_write", `{"file_path": "main.go"}`, PermAsk},
	}
	for _, c := range cases {
		got := p.Decide(c.tool, json.RawMessage(c.input))
		if got != c.expect {
			t.Errorf("%s(%s) = %v, want %v", c.tool, c.input, got, c.expect)
		}
	}
}
