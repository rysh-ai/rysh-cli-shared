package tools

import (
	"strings"
	"testing"
)

// TestShapeScreenshotContent: an empty capture is called out explicitly so
// the model never treats {"screenshot":"captured"} as an observation; a real
// capture points at the attached image; other actions pass through.
func TestShapeScreenshotContent(t *testing.T) {
	got := shapeScreenshotContent("screenshot", `{"screenshot":"captured"}`, "")
	if !strings.Contains(got, "NO image was returned") || !strings.Contains(got, "Do NOT assume") {
		t.Errorf("empty capture must warn explicitly: %q", got)
	}
	got = shapeScreenshotContent("screenshot", `{"screenshot":"captured"}`, "aGk=")
	if !strings.Contains(got, `{"screenshot":"captured"}`) || !strings.Contains(got, "attached user image") {
		t.Errorf("real capture should point at the attachment: %q", got)
	}
	if got := shapeScreenshotContent("get_text", `{"text":"hi"}`, ""); got != `{"text":"hi"}` {
		t.Errorf("non-screenshot actions must pass through: %q", got)
	}
}
