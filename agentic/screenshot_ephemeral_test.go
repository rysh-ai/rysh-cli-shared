// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/base64"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// pngB64 is a minimal valid PNG header, base64-encoded, so the media-type
// sniffer resolves image/png.
var pngB64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0000"))

func screenshotResult() *tools.ToolOutput {
	return &tools.ToolOutput{
		Content:  `{"ok":true}`,
		Metadata: map[string]string{"screenshot_base64": pngB64},
	}
}

// The stored transcript must be APPEND-ONLY across screenshot rounds: no
// image blocks stored on tool turns, no mutation of earlier turns. (Mutating
// the transcript invalidated the incremental prompt cache each round and
// burned the token budget near-quadratically.)
func TestScreenshots_StoredTranscriptStaysAppendOnlyAndImageFree(t *testing.T) {
	o := &OrchestratorActor{}
	tc := provider.ToolCallRequest{ID: "t1", Name: "browser_action", Input: []byte(`{}`)}

	turn1 := o.buildToolResultTurn(tc, screenshotResult())
	if len(turn1.ContentBlocks) != 0 {
		t.Fatalf("tool turn must NOT carry the screenshot: %+v", turn1.ContentBlocks)
	}
	if o.latestScreenshot == nil {
		t.Fatal("latestScreenshot not captured")
	}
	first := o.latestScreenshot

	// Second round replaces the latest screenshot; nothing stored either.
	turn2 := o.buildToolResultTurn(tc, screenshotResult())
	if len(turn2.ContentBlocks) != 0 {
		t.Fatalf("second tool turn must NOT carry the screenshot")
	}
	if o.latestScreenshot == first {
		t.Fatal("latestScreenshot not replaced on new capture")
	}
	if o.latestScreenshot.Source == nil || o.latestScreenshot.Source.MediaType != "image/png" {
		t.Fatalf("media type not sniffed: %+v", o.latestScreenshot)
	}
}

// requestConversation appends exactly one ephemeral screenshot turn WITHOUT
// touching the stored conversation.
func TestRequestConversation_EphemeralInjection(t *testing.T) {
	o := &OrchestratorActor{
		conversation: []provider.ConversationTurn{
			{Role: "user", Content: "go"},
			{Role: "assistant", Content: "ok"},
		},
	}

	// No screenshot: the stored slice itself (no copy needed).
	if got := o.requestConversation(); len(got) != 2 {
		t.Fatalf("want stored transcript unchanged, got %d turns", len(got))
	}

	tc := provider.ToolCallRequest{ID: "t1", Name: "browser_action", Input: []byte(`{}`)}
	_ = o.buildToolResultTurn(tc, screenshotResult())

	req := o.requestConversation()
	if len(req) != 3 {
		t.Fatalf("want 2 stored + 1 ephemeral, got %d", len(req))
	}
	eph := req[2]
	if eph.Origin != "ephemeral-screenshot" || len(eph.ContentBlocks) != 1 || eph.ContentBlocks[0].Type != "image" {
		t.Fatalf("ephemeral turn malformed: %+v", eph)
	}
	if len(o.conversation) != 2 {
		t.Fatalf("stored conversation mutated: %d turns", len(o.conversation))
	}

	// Two consecutive requests must yield identical stored prefixes (the
	// cacheable property): only the ephemeral tail may differ.
	req2 := o.requestConversation()
	for i := 0; i < 2; i++ {
		if req[i].Content != req2[i].Content || req[i].Role != req2[i].Role {
			t.Fatalf("stored prefix not stable at %d", i)
		}
	}
}
