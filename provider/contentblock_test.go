// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContentBlockText / Image constructors set the right discriminator.
func TestContentBlockConstructors(t *testing.T) {
	tb := ContentBlockText("hello")
	if tb.Type != "text" || tb.Text != "hello" {
		t.Errorf("text block: %+v", tb)
	}
	ib := ContentBlockImageBase64("image/png", "AAAB")
	if ib.Type != "image" || ib.Source == nil {
		t.Fatalf("image block: %+v", ib)
	}
	if ib.Source.Type != "base64" || ib.Source.MediaType != "image/png" || ib.Source.Data != "AAAB" {
		t.Errorf("image source: %+v", ib.Source)
	}
}

// TestBuildMessages_TextOnly: a ConversationTurn with only Content set
// emits the legacy string form for backwards compatibility.
func TestBuildMessages_TextOnly(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	msgs := c.buildMessages([]ConversationTurn{
		{Role: "user", Content: "hello"},
	})
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages: %+v", msgs)
	}
	if s, ok := msgs[0].Content.(string); !ok || s != "hello" {
		t.Errorf("expected string content, got %T %v", msgs[0].Content, msgs[0].Content)
	}
}

// TestBuildMessages_WithImage: ContentBlocks supersedes Content and
// produces the structured array form.
func TestBuildMessages_WithImage(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	msgs := c.buildMessages([]ConversationTurn{
		{Role: "user", ContentBlocks: []ContentBlock{
			ContentBlockText("why is this broken?"),
			ContentBlockImageBase64("image/png", "DEADBEEF"),
		}},
	})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	blocks, ok := msgs[0].Content.([]contentBlock)
	if !ok {
		t.Fatalf("expected []contentBlock, got %T", msgs[0].Content)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "why is this broken?" {
		t.Errorf("first block: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Data != "DEADBEEF" {
		t.Errorf("second block: %+v", blocks[1])
	}
}

// TestBuildMessages_TextPlusBlocks: when both Content and ContentBlocks
// are set, Content becomes a leading text block.
func TestBuildMessages_TextPlusBlocks(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	msgs := c.buildMessages([]ConversationTurn{
		{
			Role:    "user",
			Content: "extra prefix",
			ContentBlocks: []ContentBlock{
				ContentBlockImageBase64("image/png", "x"),
			},
		},
	})
	blocks, _ := msgs[0].Content.([]contentBlock)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "extra prefix" {
		t.Errorf("leading text block missing: %+v", blocks[0])
	}
	if blocks[1].Type != "image" {
		t.Errorf("trailing image block missing: %+v", blocks[1])
	}
}

// TestImageJSON: serialised image block matches Anthropic's expected wire
// format ({"type":"image","source":{"type":"base64","media_type":"...","data":"..."}}).
func TestImageJSON(t *testing.T) {
	cb := contentBlockFromConvBlock(ContentBlockImageBase64("image/png", "abcd"))
	b, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"type":"image"`) {
		t.Errorf("missing type=image: %s", s)
	}
	if !strings.Contains(s, `"source":`) {
		t.Errorf("missing source: %s", s)
	}
	if !strings.Contains(s, `"media_type":"image/png"`) {
		t.Errorf("missing media_type: %s", s)
	}
	if !strings.Contains(s, `"data":"abcd"`) {
		t.Errorf("missing data: %s", s)
	}
}
