package provider

import "testing"

func roles(turns []ConversationTurn) []string {
	r := make([]string, len(turns))
	for i, t := range turns {
		r[i] = t.Role
	}
	return r
}

func eqStrs(a, b []string) bool {
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

func TestHealConversationHead(t *testing.T) {
	cases := []struct {
		name string
		in   []string // roles
		want []string
	}{
		{"already user-first", []string{"user", "assistant", "tool", "user"}, []string{"user", "assistant", "tool", "user"}},
		{"leading tool dropped", []string{"tool", "user", "assistant"}, []string{"user", "assistant"}},
		{"leading assistant+tool dropped", []string{"assistant", "tool", "user", "assistant"}, []string{"user", "assistant"}},
		{"multiple leading non-user", []string{"tool", "tool", "assistant", "user"}, []string{"user"}},
		{"empty", []string{}, []string{}},
		{"degenerate no user", []string{"assistant", "tool"}, []string{"assistant", "tool"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := make([]ConversationTurn, len(c.in))
			for i, r := range c.in {
				in[i] = ConversationTurn{Role: r}
			}
			got := roles(HealConversationHead(in))
			if !eqStrs(got, c.want) {
				t.Errorf("HealConversationHead(%v) roles = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestBuildMessagesDropsOrphanedToolResultHead reproduces the exact 400 from the
// bug report: a conversation that begins with a "tool" turn (which renders as a
// user message whose first content block is a tool_result). buildMessages must
// drop that dangling head so the first emitted message is the genuine user turn.
func TestBuildMessagesDropsOrphanedToolResultHead(t *testing.T) {
	c := NewClaudeAgenticProvider("k", "https://example.invalid", "claude-x", 1024)
	conv := []ConversationTurn{
		{Role: "tool", ToolCallID: "toolu_orphan", Content: "stale result"},
		{Role: "user", Content: "which claude model are you?"},
		{Role: "assistant", Content: "I'm Claude."},
	}
	msgs := c.buildMessages(conv)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (orphan tool dropped), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want user", msgs[0].Role)
	}
	if s, ok := msgs[0].Content.(string); !ok || s != "which claude model are you?" {
		t.Errorf("first message content = %#v, want the user prompt string", msgs[0].Content)
	}
}
