package provider

import "testing"

func countBreakpoints(msgs []agenticMessage) int {
	n := 0
	for _, m := range msgs {
		if blocks, ok := m.Content.([]contentBlock); ok {
			for _, b := range blocks {
				if b.CacheControl != nil {
					n++
				}
			}
		}
	}
	return n
}

// The trailing breakpoint must mark the last TWO messages: the final message
// can be ephemeral (current-screenshot injection, changes every round), so
// the second-to-last carries the boundary of the stable stored prefix that
// the next request actually re-reads from the cache.
func TestApplyTrailingMessageCacheBreakpoint_LastTwo(t *testing.T) {
	msgs := []agenticMessage{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: []contentBlock{{Type: "text", Text: "two"}}},
		{Role: "user", Content: "three"},
	}
	applyTrailingMessageCacheBreakpoint(msgs)
	if got := countBreakpoints(msgs); got != 2 {
		t.Fatalf("want 2 breakpoints (last two messages), got %d", got)
	}
	// First message untouched.
	if _, converted := msgs[0].Content.([]contentBlock); converted {
		t.Fatal("first message should not be touched")
	}
	// String content converted to a marked text block.
	blocks, ok := msgs[2].Content.([]contentBlock)
	if !ok || blocks[0].CacheControl == nil || blocks[0].Text != "three" {
		t.Fatalf("last message not marked correctly: %+v", msgs[2].Content)
	}
}

func TestApplyTrailingMessageCacheBreakpoint_SingleAndEmpty(t *testing.T) {
	applyTrailingMessageCacheBreakpoint(nil) // must not panic
	one := []agenticMessage{{Role: "user", Content: "only"}}
	applyTrailingMessageCacheBreakpoint(one)
	if got := countBreakpoints(one); got != 1 {
		t.Fatalf("single message: want 1 breakpoint, got %d", got)
	}
}
