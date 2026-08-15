// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildSystemBlocks_AttachesCacheControl(t *testing.T) {
	blocks := buildSystemBlocks("you are an assistant")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(blocks))
	}
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("expected ephemeral cache_control on system block")
	}
	if got := buildSystemBlocks(""); got != nil {
		t.Fatalf("expected nil for empty system prompt, got %v", got)
	}
}

func TestApplyToolCacheBreakpoint_LastToolOnly(t *testing.T) {
	defs := []agenticToolDef{{Name: "a"}, {Name: "b"}}
	applyToolCacheBreakpoint(defs)
	if defs[0].CacheControl != nil {
		t.Errorf("first tool should not carry cache_control")
	}
	if defs[1].CacheControl == nil {
		t.Errorf("last tool should carry cache_control")
	}
}

func TestApplyTrailingMessageCacheBreakpoint_ConvertsString(t *testing.T) {
	msgs := []agenticMessage{{Role: "user", Content: "hello"}}
	applyTrailingMessageCacheBreakpoint(msgs)
	blocks, ok := msgs[0].Content.([]contentBlock)
	if !ok {
		t.Fatalf("expected string content converted to []contentBlock, got %T", msgs[0].Content)
	}
	if len(blocks) != 1 || blocks[0].CacheControl == nil {
		t.Fatalf("expected single text block with cache_control")
	}

	// Marshalled request must contain cache_control.
	b, _ := json.Marshal(agenticRequest{System: buildSystemBlocks("sp"), Messages: msgs})
	if !strings.Contains(string(b), "cache_control") {
		t.Errorf("marshalled request missing cache_control: %s", b)
	}
}

func TestParseResponse_CapturesUsage(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	body := &agenticResponseBody{
		StopReason: "end_turn",
		Usage: &agenticUsage{
			InputTokens:              10,
			OutputTokens:             20,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 5,
		},
	}
	resp := c.parseResponse(body)
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
	if got := resp.Usage.TotalInputTokens(); got != 45 {
		t.Errorf("TotalInputTokens = %d, want 45", got)
	}
}

// TestUsageNewTokens verifies the token-budget accounting counts only genuinely
// new tokens (uncached input + cache-writes + output) and EXCLUDES cache reads —
// so summing across calls doesn't re-count the re-sent conversation prefix.
func TestUsageNewTokens(t *testing.T) {
	// A typical mid-run call: a large cached prefix re-read, a small new delta,
	// and some output. NewTokens must ignore the 40k cache read.
	u := Usage{InputTokens: 800, CacheReadInputTokens: 40000, CacheCreationInputTokens: 1200, OutputTokens: 500}
	if got, want := u.NewTokens(), 800+1200+500; got != want {
		t.Errorf("NewTokens = %d, want %d (excludes cache reads)", got, want)
	}
	// TotalInputTokens still includes cache reads (used for compaction, not budget).
	if got := u.TotalInputTokens(); got != 800+40000+1200 {
		t.Errorf("TotalInputTokens = %d, want 42000", got)
	}
	// Cumulative NewTokens across 3 identical calls stays linear (~3x the delta),
	// not 3x the full 42k context — the whole point of the fix.
	cumulativeNew := u.NewTokens() * 3
	cumulativeTotal := u.TotalInputTokens() * 3
	if cumulativeNew >= cumulativeTotal/10 {
		t.Errorf("NewTokens should be far below TotalInputTokens under caching: new=%d total=%d", cumulativeNew, cumulativeTotal)
	}
}
