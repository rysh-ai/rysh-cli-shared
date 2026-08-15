// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

func mkTurns(roles ...string) []provider.ConversationTurn {
	ts := make([]provider.ConversationTurn, len(roles))
	for i, r := range roles {
		ts[i] = provider.ConversationTurn{Role: r}
	}
	return ts
}

// TestBoundConversationHealsShortHead covers a small (untrimmed) conversation
// whose head was left as an orphaned tool_result — e.g. restored from KV in
// that broken state. boundConversation must heal it even when no trim happens.
func TestBoundConversationHealsShortHead(t *testing.T) {
	got := boundConversation(mkTurns("tool", "user", "assistant"))
	if len(got) != 2 || got[0].Role != "user" {
		t.Fatalf("got roles %v, want [user assistant]", rolesOf(got))
	}
}

// TestBoundConversationTrimAndHeal covers the reported failure mode: a long
// conversation whose tail cut (len-50) lands on a "tool" turn. After trimming
// to the last 50 turns the head must be advanced past the dangling tool_result
// to the first genuine user turn.
func TestBoundConversationTrimAndHeal(t *testing.T) {
	roles := make([]string, 52)
	for i := range roles {
		roles[i] = "assistant"
	}
	roles[2] = "tool" // tail cut starts here (52-50=2): the orphan
	roles[3] = "user" // first genuine user turn after the cut
	roles[51] = "user"

	got := boundConversation(mkTurns(roles...))
	if len(got) == 0 || got[0].Role != "user" {
		t.Fatalf("head not healed: got %d turns, head=%q", len(got), headRole(got))
	}
	// Cut at 2 (tool), healed forward to the user at index 3 → 52-3 = 49 turns.
	if len(got) != 49 {
		t.Errorf("len = %d, want 49", len(got))
	}
}

// TestBoundConversationLeavesValidHead is a no-op guard: a user-first window is
// returned unchanged (modulo trimming).
func TestBoundConversationLeavesValidHead(t *testing.T) {
	in := mkTurns("user", "assistant", "tool", "user")
	got := boundConversation(in)
	if len(got) != 4 || got[0].Role != "user" {
		t.Errorf("got roles %v, want unchanged", rolesOf(got))
	}
}

func rolesOf(turns []provider.ConversationTurn) []string {
	r := make([]string, len(turns))
	for i, t := range turns {
		r[i] = t.Role
	}
	return r
}

func headRole(turns []provider.ConversationTurn) string {
	if len(turns) == 0 {
		return "<empty>"
	}
	return turns[0].Role
}
