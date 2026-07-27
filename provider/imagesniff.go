package provider

// Image media-type sniffing for base64 payloads.
//
// Screenshot producers disagree about encoding: the headless CDP path emits
// PNG (Page.captureScreenshot default) while the Electron embedded-browser
// path emits JPEG (toJPEG(q40) to stay under the NATS ~1 MB message limit).
// The Anthropic API validates the declared media_type against the actual
// magic bytes and 400s on mismatch, so the daemon must never hardcode a
// media type for bytes it did not produce — it sniffs them instead.

import (
	"encoding/base64"
	"strings"
)

// StripImageDataURLPrefix removes a leading `data:image/...;base64,` (or any
// `data:*;base64,`) prefix so the remainder is raw base64 suitable for an
// Anthropic image source block. Input without a dataURL prefix is returned
// unchanged.
func StripImageDataURLPrefix(s string) string {
	if !strings.HasPrefix(s, "data:") {
		return s
	}
	if idx := strings.Index(s, ";base64,"); idx != -1 {
		return s[idx+len(";base64,"):]
	}
	// Non-base64 dataURL (e.g. "data:image/svg+xml,<svg…>") — nothing usable
	// for a base64 image block; return unchanged and let the sniffer fall
	// back.
	return s
}

// DetectImageMediaType decodes the first few bytes of a base64 image payload
// and returns the media type indicated by its magic bytes:
//
//	PNG:  \x89PNG
//	JPEG: \xFF\xD8\xFF
//	WebP: RIFF....WEBP
//	GIF:  GIF8
//
// A leading dataURL prefix is tolerated (stripped before sniffing).
// Returns ("", false) when the payload is unrecognized or undecodable.
func DetectImageMediaType(b64 string) (string, bool) {
	b64 = StripImageDataURLPrefix(b64)
	// WebP needs bytes 0-3 ("RIFF") and 8-11 ("WEBP") → decode ≥12 bytes.
	// 24 base64 chars decode to 18 bytes; shorter (but multiple-of-4) inputs
	// still decode for the 3-4 byte signatures.
	head := b64
	if len(head) > 24 {
		head = head[:24]
	}
	head = head[:len(head)-len(head)%4]
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil || len(raw) < 4 {
		return "", false
	}
	switch {
	case raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G':
		return "image/png", true
	case raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "image/jpeg", true
	case len(raw) >= 12 &&
		raw[0] == 'R' && raw[1] == 'I' && raw[2] == 'F' && raw[3] == 'F' &&
		raw[8] == 'W' && raw[9] == 'E' && raw[10] == 'B' && raw[11] == 'P':
		return "image/webp", true
	case raw[0] == 'G' && raw[1] == 'I' && raw[2] == 'F' && raw[3] == '8':
		return "image/gif", true
	default:
		return "", false
	}
}

// SniffImageMediaType is DetectImageMediaType with an "image/png" fallback —
// the historical default, so behavior is unchanged for payloads the sniffer
// cannot read.
func SniffImageMediaType(b64 string) string {
	if mt, ok := DetectImageMediaType(b64); ok {
		return mt
	}
	return "image/png"
}

// HealImageBlock returns cb with a base64 image source's Data stripped of any
// dataURL prefix and its declared MediaType corrected to the sniffed type
// when the magic bytes are recognized. Non-image blocks, non-base64 sources,
// and empty payloads pass through untouched, as does a declared media type
// the sniffer cannot verify.
//
// This heals conversations persisted BEFORE media types were sniffed at
// capture time: a turn restored from session KV may still carry e.g. JPEG
// bytes labeled "image/png", and re-sending it 400s deterministically until
// corrected. buildMessages applies this at the single chokepoint every
// request passes through.
func HealImageBlock(cb ContentBlock) ContentBlock {
	if cb.Type != "image" || cb.Source == nil || cb.Source.Type != "base64" || cb.Source.Data == "" {
		return cb
	}
	data := StripImageDataURLPrefix(cb.Source.Data)
	mediaType := cb.Source.MediaType
	if detected, ok := DetectImageMediaType(data); ok {
		mediaType = detected
	} else if mediaType == "" {
		mediaType = "image/png"
	}
	if data == cb.Source.Data && mediaType == cb.Source.MediaType {
		return cb
	}
	src := *cb.Source
	src.Data = data
	src.MediaType = mediaType
	cb.Source = &src
	return cb
}
