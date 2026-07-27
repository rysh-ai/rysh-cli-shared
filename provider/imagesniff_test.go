package provider

import (
	"encoding/base64"
	"testing"
)

// b64 encodes raw magic-byte prefixes (padded so the payload is plausibly
// image-sized) the same way producers do: standard base64, no line breaks.
func b64(magic []byte) string {
	padded := make([]byte, 32)
	copy(padded, magic)
	return base64.StdEncoding.EncodeToString(padded)
}

func TestSniffImageMediaType(t *testing.T) {
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	webpMagic := []byte{'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'}
	gifMagic := []byte("GIF89a")

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"png", b64(pngMagic), "image/png"},
		{"jpeg", b64(jpegMagic), "image/jpeg"},
		{"webp", b64(webpMagic), "image/webp"},
		{"gif", b64(gifMagic), "image/gif"},
		// Electron's toJPEG output starts with the canonical /9j/ prefix.
		{"jpeg canonical base64 prefix", "/9j/4AAQSkZJRgABAQAAAQABAAD", "image/jpeg"},
		// CDP PNG output starts with the canonical iVBORw0KGgo prefix.
		{"png canonical base64 prefix", "iVBORw0KGgoAAAANSUhEUgAAAAE", "image/png"},
		{"dataURL-prefixed jpeg", "data:image/jpeg;base64," + b64(jpegMagic), "image/jpeg"},
		{"dataURL-prefixed png", "data:image/png;base64," + b64(pngMagic), "image/png"},
		{"unknown magic falls back to png", b64([]byte{0x00, 0x01, 0x02, 0x03}), "image/png"},
		{"empty input falls back to png", "", "image/png"},
		{"invalid base64 falls back to png", "!!!not-base64!!!", "image/png"},
		{"too-short input falls back to png", "QQ==", "image/png"}, // decodes to 1 byte
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SniffImageMediaType(tc.input); got != tc.want {
				t.Errorf("SniffImageMediaType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestHealImageBlock(t *testing.T) {
	jpegData := b64([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	pngData := b64([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})

	t.Run("corrects mislabeled jpeg declared as png", func(t *testing.T) {
		in := ContentBlockImageBase64("image/png", jpegData)
		out := HealImageBlock(in)
		if out.Source.MediaType != "image/jpeg" {
			t.Errorf("MediaType = %q, want image/jpeg", out.Source.MediaType)
		}
		// The input block must not be mutated (persisted turns are shared).
		if in.Source.MediaType != "image/png" {
			t.Errorf("input block mutated: MediaType = %q", in.Source.MediaType)
		}
	})

	t.Run("keeps correct declaration untouched", func(t *testing.T) {
		in := ContentBlockImageBase64("image/png", pngData)
		out := HealImageBlock(in)
		if out.Source != in.Source {
			t.Error("expected pass-through for an already-correct block")
		}
	})

	t.Run("strips dataURL prefix", func(t *testing.T) {
		in := ContentBlockImageBase64("image/png", "data:image/jpeg;base64,"+jpegData)
		out := HealImageBlock(in)
		if out.Source.Data != jpegData {
			t.Errorf("Data = %q, want prefix stripped", out.Source.Data)
		}
		if out.Source.MediaType != "image/jpeg" {
			t.Errorf("MediaType = %q, want image/jpeg", out.Source.MediaType)
		}
	})

	t.Run("unverifiable payload keeps declared type", func(t *testing.T) {
		in := ContentBlockImageBase64("image/jpeg", "!!!not-base64!!!")
		out := HealImageBlock(in)
		if out.Source.MediaType != "image/jpeg" {
			t.Errorf("MediaType = %q, want declared image/jpeg kept", out.Source.MediaType)
		}
	})

	t.Run("empty declared type falls back to png when unverifiable", func(t *testing.T) {
		in := ContentBlockImageBase64("", "!!!not-base64!!!")
		out := HealImageBlock(in)
		if out.Source.MediaType != "image/png" {
			t.Errorf("MediaType = %q, want image/png fallback", out.Source.MediaType)
		}
	})

	t.Run("non-image and non-base64 blocks pass through", func(t *testing.T) {
		text := ContentBlockText("hello")
		if out := HealImageBlock(text); out.Text != "hello" || out.Type != "text" {
			t.Error("text block altered")
		}
		urlBlock := ContentBlock{Type: "image", Source: &ImageSource{Type: "url", URL: "https://x/y.png"}}
		if out := HealImageBlock(urlBlock); out.Source != urlBlock.Source {
			t.Error("url image block altered")
		}
	})
}

// TestBuildMessagesHealsToolTurnImages exercises the buildMessages chokepoint:
// a tool turn restored from session KV with a JPEG screenshot mislabeled as
// PNG (the pre-sniffer capture bug) must reach the wire with the corrected
// media type — this is what un-bricks resumed runs.
func TestBuildMessagesHealsToolTurnImages(t *testing.T) {
	jpegData := b64([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	c := &ClaudeAgenticProvider{}
	conversation := []ConversationTurn{
		{Role: "user", Content: "look at the page"},
		{Role: "assistant", ToolCalls: []ToolCallRequest{{ID: "tu_1", Name: "browser_action"}}},
		{
			Role:       "tool",
			ToolCallID: "tu_1",
			Content:    "screenshot captured",
			ContentBlocks: []ContentBlock{
				ContentBlockImageBase64("image/png", jpegData), // stale label
			},
		},
	}
	messages := c.buildMessages(conversation)
	// Last message is the follow-up user message carrying the image.
	last := messages[len(messages)-1]
	blocks, ok := last.Content.([]ContentBlock)
	if !ok {
		t.Fatalf("unexpected content type %T", last.Content)
	}
	if got := blocks[0].Source.MediaType; got != "image/jpeg" {
		t.Errorf("wire media type = %q, want image/jpeg", got)
	}
}

func TestStripImageDataURLPrefix(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"strips image/png dataURL", "data:image/png;base64,iVBORw0KGgo", "iVBORw0KGgo"},
		{"strips image/jpeg dataURL", "data:image/jpeg;base64,/9j/4AAQ", "/9j/4AAQ"},
		{"plain base64 unchanged", "/9j/4AAQ", "/9j/4AAQ"},
		{"empty unchanged", "", ""},
		{"non-base64 dataURL unchanged", "data:image/svg+xml,<svg/>", "data:image/svg+xml,<svg/>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripImageDataURLPrefix(tc.input); got != tc.want {
				t.Errorf("StripImageDataURLPrefix(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
