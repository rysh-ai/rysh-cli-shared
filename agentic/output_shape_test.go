package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestShapeToolOutput_NoTruncation exercises the small-output path: metadata
// is added but Content is unchanged.
func TestShapeToolOutput_NoTruncation(t *testing.T) {
	out := &tools.ToolOutput{Content: "small output\nsecond line\n"}
	got := shapeToolOutput(out)
	if got != out {
		t.Fatalf("expected same pointer back")
	}
	if got.Content != "small output\nsecond line\n" {
		t.Errorf("content mutated unexpectedly: %q", got.Content)
	}
	if got.Metadata["byte_count"] == "" || got.Metadata["line_count"] == "" {
		t.Errorf("expected size metadata, got %v", got.Metadata)
	}
	if _, truncated := got.Metadata["truncated"]; truncated {
		t.Errorf("did not expect 'truncated' marker for small output")
	}
}

// TestShapeToolOutput_HeadTail forces line-based truncation by exceeding the
// line cap with cheap repetitive content.
func TestShapeToolOutput_HeadTail(t *testing.T) {
	// Temporarily tighten the line cap so the test stays fast and small.
	origLineCap := MaxToolOutputLines
	origHead := ToolOutputHeadLines
	origTail := ToolOutputTailLines
	MaxToolOutputLines = 20
	ToolOutputHeadLines = 5
	ToolOutputTailLines = 5
	defer func() {
		MaxToolOutputLines = origLineCap
		ToolOutputHeadLines = origHead
		ToolOutputTailLines = origTail
	}()

	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString("line ")
		sb.WriteString(string(rune('A' + (i % 26))))
		sb.WriteByte('\n')
	}
	out := shapeToolOutput(&tools.ToolOutput{Content: sb.String()})

	if out.Metadata["truncated"] != "true" {
		t.Errorf("expected truncated=true, got %v", out.Metadata)
	}
	if out.Metadata["truncation_mode"] != "head_tail" {
		t.Errorf("expected head_tail mode, got %q", out.Metadata["truncation_mode"])
	}
	if !strings.Contains(out.Content, "lines / ") || !strings.Contains(out.Content, "bytes omitted") {
		t.Errorf("missing omission marker in:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "line A") {
		t.Errorf("head not preserved: %q", out.Content)
	}
	// The last 5 lines correspond to i=95..99 → chars 'R','S','T','U','V'.
	if !strings.Contains(out.Content, "line V") {
		t.Errorf("tail not preserved: %q", out.Content)
	}
}

// TestShapeToolOutput_ByteFallback exercises the byte-based fallback path
// when content is one giant line over the byte cap.
func TestShapeToolOutput_ByteFallback(t *testing.T) {
	origByte := MaxToolOutputBytes
	MaxToolOutputBytes = 100
	defer func() { MaxToolOutputBytes = origByte }()

	huge := strings.Repeat("x", 1000)
	out := shapeToolOutput(&tools.ToolOutput{Content: huge})
	if out.Metadata["truncated"] != "true" {
		t.Errorf("expected truncated=true, got %v", out.Metadata)
	}
	if out.Metadata["truncation_mode"] != "bytes" {
		t.Errorf("expected bytes mode, got %q", out.Metadata["truncation_mode"])
	}
	if !strings.Contains(out.Content, "bytes omitted") {
		t.Errorf("missing byte omission marker in: %q", out.Content)
	}
	if len(out.Content) >= 1000 {
		t.Errorf("byte truncate didn't shrink content: %d bytes", len(out.Content))
	}
}

// TestShapeToolOutput_NilSafe ensures nil input doesn't panic.
func TestShapeToolOutput_NilSafe(t *testing.T) {
	if got := shapeToolOutput(nil); got != nil {
		t.Errorf("nil input should produce nil output")
	}
}

// TestShapeToolOutput_EmptyContent ensures zero-byte content gets sane metadata.
func TestShapeToolOutput_EmptyContent(t *testing.T) {
	out := shapeToolOutput(&tools.ToolOutput{Content: ""})
	if out.Metadata["byte_count"] != "0" || out.Metadata["line_count"] != "0" {
		t.Errorf("expected zero counts, got %v", out.Metadata)
	}
	if _, ok := out.Metadata["truncated"]; ok {
		t.Errorf("empty content should not be marked truncated")
	}
}

// TestShapeToolOutput_PreservesExistingMetadata: a tool that already wrote
// metadata.line_count or byte_count keeps its own value.
func TestShapeToolOutput_PreservesExistingMetadata(t *testing.T) {
	out := shapeToolOutput(&tools.ToolOutput{
		Content:  "hi",
		Metadata: map[string]string{"byte_count": "999", "line_count": "999", "extra": "kept"},
	})
	if out.Metadata["byte_count"] != "999" {
		t.Errorf("expected byte_count preserved, got %q", out.Metadata["byte_count"])
	}
	if out.Metadata["line_count"] != "999" {
		t.Errorf("expected line_count preserved, got %q", out.Metadata["line_count"])
	}
	if out.Metadata["extra"] != "kept" {
		t.Errorf("expected extra preserved, got %q", out.Metadata["extra"])
	}
}
