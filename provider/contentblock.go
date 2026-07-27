package provider

// Follow-up item 1 — multimodal input (image content blocks).
//
// ConversationTurn.Content (string) is the path text-only callers use.
// When a caller needs to attach images, they populate ContentBlocks
// instead; provider.buildMessages emits the structured Anthropic format.
//
// MVP: base64-inline images. URL and File-ID sources are scaffolded
// but not exercised by rysh-cli's input paths yet.

// ContentBlock is one structured content element in a user/assistant
// message. Use ContentBlockText / ContentBlockImage constructors to
// avoid hand-rolling the discriminator field.
type ContentBlock struct {
	Type   string       `json:"type"`             // "text" | "image"
	Text   string       `json:"text,omitempty"`   // when Type == "text"
	Source *ImageSource `json:"source,omitempty"` // when Type == "image"
}

// ImageSource carries the payload for an image content block. Anthropic
// accepts base64-inline (Type "base64"), URL (Type "url"), or File-ID
// (Type "file_id"). MVP only exercises "base64".
type ImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url" | "file_id"
	MediaType string `json:"media_type,omitempty"` // e.g. "image/png"
	Data      string `json:"data,omitempty"`       // base64-encoded when Type=="base64"
	URL       string `json:"url,omitempty"`        // when Type=="url"
	FileID    string `json:"file_id,omitempty"`    // when Type=="file_id"
}

// ContentBlockText is a convenience constructor for text blocks.
func ContentBlockText(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ContentBlockImageBase64 is a convenience constructor for inline base64
// image blocks. mediaType is e.g. "image/png".
func ContentBlockImageBase64(mediaType, data string) ContentBlock {
	return ContentBlock{
		Type: "image",
		Source: &ImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      data,
		},
	}
}
