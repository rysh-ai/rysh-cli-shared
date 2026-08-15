// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDefaultThinkingConfig pins the defaults so accidental drops are caught.
// The modern default is adaptive (Claude 4.6+); the legacy fixed-budget form
// is only produced by LegacyThinkingConfig.
func TestDefaultThinkingConfig(t *testing.T) {
	c := DefaultThinkingConfig()
	if c.Type != "adaptive" {
		t.Errorf("type = %q, want adaptive", c.Type)
	}
	if c.BudgetTokens != 0 {
		t.Errorf("adaptive config must not carry a budget, got %d", c.BudgetTokens)
	}

	l := LegacyThinkingConfig(0)
	if l.Type != "enabled" {
		t.Errorf("legacy type = %q, want enabled", l.Type)
	}
	if l.BudgetTokens <= 0 {
		t.Errorf("legacy budget should be positive, got %d", l.BudgetTokens)
	}
}

// TestParseResponse_ThinkingBlock: parseResponse should surface thinking
// content blocks as ThinkingBlocks on the response.
func TestParseResponse_ThinkingBlock(t *testing.T) {
	c := &ClaudeAgenticProvider{model: "claude-test"}
	body := &agenticResponseBody{
		Content: []agenticContentBlock{
			{Type: "thinking", Thinking: "let me think...", Signature: "sig-abc"},
			{Type: "text", Text: "Answer is 42."},
		},
		StopReason: "end_turn",
	}
	resp := c.parseResponse(body)
	if len(resp.ThinkingBlocks) != 1 {
		t.Fatalf("want 1 thinking block, got %d", len(resp.ThinkingBlocks))
	}
	if resp.ThinkingBlocks[0].Text != "let me think..." {
		t.Errorf("thinking text = %q", resp.ThinkingBlocks[0].Text)
	}
	if resp.ThinkingBlocks[0].Signature != "sig-abc" {
		t.Errorf("thinking sig = %q", resp.ThinkingBlocks[0].Signature)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Answer is 42." {
		t.Errorf("text not preserved: %+v", resp.TextBlocks)
	}
}

// TestRequest_ThinkingMarshalling: agenticRequest.Thinking marshals when
// set, and is omitted entirely when nil.
func TestRequest_ThinkingMarshalling(t *testing.T) {
	r := agenticRequest{
		Model:    "claude-x",
		Messages: nil,
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "thinking") {
		t.Errorf("nil thinking should be omitted: %s", string(b))
	}
	r.Thinking = DefaultThinkingConfig()
	b, _ = json.Marshal(r)
	if !strings.Contains(string(b), `"thinking":{"type":"adaptive"}`) {
		t.Errorf("adaptive thinking should be present without budget_tokens: %s", string(b))
	}

	r.Thinking = LegacyThinkingConfig(8192)
	b, _ = json.Marshal(r)
	if !strings.Contains(string(b), `"budget_tokens":`) {
		t.Errorf("legacy budget_tokens should be present: %s", string(b))
	}
}

// TestParseClaudeStream_ThinkingDelta drives the SSE parser through a
// thinking block: content_block_start (thinking) → thinking_delta x2 →
// signature_delta → content_block_stop → message_stop. Verifies that the
// assembled response has a ThinkingBlock with the joined text and signature.
func TestParseClaudeStream_ThinkingDelta(t *testing.T) {
	body := fakeSSE(
		[2]string{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Step 1: "}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"factor it."}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer: 7"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	var thinkingDeltas []string
	resp, err := parseClaudeStream(strings.NewReader(body), func(ev StreamEvent) {
		if ev.Type == StreamEventThinkingDelta {
			thinkingDeltas = append(thinkingDeltas, ev.Thinking)
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.ThinkingBlocks) != 1 {
		t.Fatalf("want 1 thinking block, got %d", len(resp.ThinkingBlocks))
	}
	if resp.ThinkingBlocks[0].Text != "Step 1: factor it." {
		t.Errorf("thinking text = %q", resp.ThinkingBlocks[0].Text)
	}
	if resp.ThinkingBlocks[0].Signature != "sig-xyz" {
		t.Errorf("signature = %q", resp.ThinkingBlocks[0].Signature)
	}
	if got := strings.Join(thinkingDeltas, "|"); got != "Step 1: |factor it." {
		t.Errorf("deltas = %q", got)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "Answer: 7" {
		t.Errorf("text block not parsed: %+v", resp.TextBlocks)
	}
}
