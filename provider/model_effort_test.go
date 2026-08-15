// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestWithModelEffort covers the per-run override wrapper: non-empty fields
// replace, empty fields keep, the receiver is never mutated, and the effort
// lands in the request body as output_config.effort.
func TestWithModelEffort(t *testing.T) {
	base := NewClaudeAgenticProvider("k", "", "claude-opus-4-8", 1024)

	over := base.WithModelEffort("claude-haiku-4-5", "low").(*ClaudeAgenticProvider)
	if over.model != "claude-haiku-4-5" || over.effort != "low" {
		t.Errorf("override not applied: model=%q effort=%q", over.model, over.effort)
	}
	if base.model != "claude-opus-4-8" || base.effort != "" {
		t.Errorf("receiver mutated: model=%q effort=%q", base.model, base.effort)
	}

	// Empty override fields keep the receiver's values.
	kept := over.WithModelEffort("", "").(*ClaudeAgenticProvider)
	if kept.model != "claude-haiku-4-5" || kept.effort != "low" {
		t.Errorf("empty overrides should keep values: %+v", kept)
	}

	// outputConfig: nil without effort (request body unchanged), set with it.
	if base.outputConfig() != nil {
		t.Error("no effort should keep output_config nil")
	}
	b, _ := json.Marshal(agenticRequest{Model: over.model, OutputConfig: over.outputConfig()})
	if !strings.Contains(string(b), `"output_config":{"effort":"low"}`) {
		t.Errorf("effort missing from request JSON: %s", b)
	}
}

// TestToolTurnScreenshotMessage covers the vision delivery: a tool turn with
// an image ContentBlock marshals into the tool_result message FOLLOWED by a
// user message carrying the image block.
func TestToolTurnScreenshotMessage(t *testing.T) {
	turns := []ConversationTurn{
		{Role: "user", Content: "look at the page"},
		{Role: "assistant", Content: "capturing", ToolCalls: []ToolCallRequest{
			{ID: "t1", Name: "browser_action", Input: []byte(`{}`)}}},
		{Role: "tool", ToolCallID: "t1", Content: `{"screenshot":"captured"}`,
			ContentBlocks: []ContentBlock{{Type: "image", Source: &ImageSource{
				Type: "base64", MediaType: "image/png", Data: "aGk="}}}},
	}
	msgs := NewClaudeAgenticProvider("k", "", "m", 10).buildMessages(turns)
	if len(msgs) != 4 { // user text, assistant tool_use, tool_result, follow-up image message
		t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
	}
	b, _ := json.Marshal(msgs)
	s := string(b)
	if !strings.Contains(s, `"tool_result"`) || !strings.Contains(s, `"data":"aGk="`) {
		t.Errorf("blocks missing: %s", s)
	}
	if strings.Index(s, `"image"`) < strings.Index(s, `"tool_result"`) {
		t.Errorf("image must follow tool_result: %s", s)
	}
	// Without attachments: no follow-up message. (The tool turn needs its
	// owning assistant tool_use — an orphaned tool_result is healed away.)
	plain := NewClaudeAgenticProvider("k", "", "m", 10).buildMessages([]ConversationTurn{
		{Role: "assistant", ToolCalls: []ToolCallRequest{
			{ID: "t1", Name: "bash", Input: []byte(`{}`)}}},
		{Role: "tool", ToolCallID: "t1", Content: "ok"},
	})
	if len(plain) != 2 {
		t.Errorf("assistant + plain tool turn should emit two messages, got %d", len(plain))
	}
}

// TestIsEffortUnsupportedErr matches the API's effort-rejection shape only.
func TestIsEffortUnsupportedErr(t *testing.T) {
	err := errFor("claude-agentic: invalid_request_error (HTTP 400): This model does not support the effort parameter.")
	if !IsEffortUnsupportedErr(err) {
		t.Error("should match the effort-unsupported 400")
	}
	if IsEffortUnsupportedErr(errFor("rate_limit_error (HTTP 429)")) || IsEffortUnsupportedErr(nil) {
		t.Error("must not match other errors or nil")
	}
}

func errFor(msg string) error { return errors.New(msg) }
