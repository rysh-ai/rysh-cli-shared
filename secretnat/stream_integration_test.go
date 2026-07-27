package secretnat

import (
	"context"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// splitStreamer is a fake StreamingProvider that emits its canned text as
// StreamEventTextDelta events of `chunk` bytes each — simulating a synthetic
// token straddling SSE delta boundaries.
type splitStreamer struct {
	spyProvider
	text  string
	chunk int
}

func (p *splitStreamer) CompleteWithToolsStream(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
	cb provider.StreamCallback,
) (*provider.AgenticResponse, error) {
	for i := 0; i < len(p.text); i += p.chunk {
		end := i + p.chunk
		if end > len(p.text) {
			end = len(p.text)
		}
		cb(provider.StreamEvent{Type: provider.StreamEventTextDelta, Text: p.text[i:end]})
	}
	cb(provider.StreamEvent{Type: provider.StreamEventMessageStop})
	return p.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// TestStreamedDisplayRestoreComposition is the end-to-end composition the
// orchestrator performs for restore_display: the NAT-wrapped streaming
// provider sanitizes the outbound conversation, the response streams a
// synthetic token split across delta events, and the display-side
// StreamRestorer (Feed per delta + Flush at stream end) reassembles and
// restores it.
func TestStreamedDisplayRestoreComposition(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true, RestoreDisplay: true})
	s := m.Session("pane-1")

	// The user turn mints the token (exactly as startPrompt's sanitize would).
	userTurn := s.Sanitize("my key is " + githubKey)
	tok := strings.TrimPrefix(userTurn, "my key is ")

	for _, chunk := range []int{1, 2, 3, 5, len(tok) + 7} {
		inner := &splitStreamer{text: "the key you gave me is " + tok + " — noted", chunk: chunk}
		wrapped := Wrap(inner, s).(provider.StreamingProvider)

		// Orchestrator-style display pipeline: Feed each delta, Flush at end.
		restorer := s.NewStreamRestorer()
		var display strings.Builder
		conv := []provider.ConversationTurn{{Role: "user", Content: userTurn}}
		_, err := wrapped.CompleteWithToolsStream(context.Background(), conv, nil, "sys",
			func(ev provider.StreamEvent) {
				if ev.Type == provider.StreamEventTextDelta && ev.Text != "" {
					display.WriteString(restorer.Feed(ev.Text))
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		display.WriteString(restorer.Flush())

		want := "the key you gave me is " + githubKey + " — noted"
		if got := display.String(); got != want {
			t.Fatalf("chunk=%d: display = %q, want %q", chunk, got, want)
		}
		// And the wire request stayed synthetic.
		for _, turn := range inner.lastConv {
			if strings.Contains(turn.Content, githubKey) {
				t.Fatalf("chunk=%d: outbound conversation leaked the real key", chunk)
			}
		}
	}
}
