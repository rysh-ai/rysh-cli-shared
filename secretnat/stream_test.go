package secretnat

import (
	"strings"
	"testing"
)

// mintToken sanitizes value in the session and returns the minted token.
func mintToken(t *testing.T, s *Session, value string) string {
	t.Helper()
	tok := s.Sanitize(value)
	if tok == value {
		t.Fatalf("no token minted for %q", value)
	}
	return tok
}

// feedAll streams text through r in the given chunk sizes and returns the
// concatenated output incl. Flush.
func feedAll(r *StreamRestorer, text string, chunk int) string {
	var out strings.Builder
	for i := 0; i < len(text); i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		out.WriteString(r.Feed(text[i:end]))
	}
	out.WriteString(r.Flush())
	return out.String()
}

func TestStreamRestorerSplitToken(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	tok := mintToken(t, s, githubKey)
	text := "your key is " + tok + " — keep it safe"

	// Every chunking granularity must restore identically.
	want := "your key is " + githubKey + " — keep it safe"
	for _, chunk := range []int{1, 2, 3, 5, 7, 100, len(text)} {
		got := feedAll(s.NewStreamRestorer(), text, chunk)
		if got != want {
			t.Fatalf("chunk=%d: got %q, want %q", chunk, got, want)
		}
	}
}

func TestStreamRestorerKnownRefSplit(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	m.UpdateKnownSecrets([]KnownSecret{{Name: "STRIPE_KEY", Value: stripeKey}})
	s := m.Session("pane-1")
	text := "use ${STRIPE_KEY} for billing"
	want := "use " + stripeKey + " for billing"
	for _, chunk := range []int{1, 3, 4, 6} {
		got := feedAll(s.NewStreamRestorer(), text, chunk)
		if got != want {
			t.Fatalf("chunk=%d: got %q, want %q", chunk, got, want)
		}
	}
}

func TestStreamRestorerNearMissNeverCompletes(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	mintToken(t, s, githubKey) // token ghp_SNAT000001 now restorable

	// "ghp_SNAT00" prose that never completes into the token must still be
	// emitted (on Flush at the latest) and unmodified.
	text := "prefix ghp_SNAT00 and done"
	got := feedAll(s.NewStreamRestorer(), text, 4)
	if got != text {
		t.Fatalf("near-miss mangled: %q", got)
	}

	// Plain "SNAT" in prose is not a token prefix (tokens start with their
	// format prefix) and must stream through with zero hold-back.
	r := s.NewStreamRestorer()
	if out := r.Feed("The SNAT protocol is fine"); out != "The SNAT protocol is fine" {
		t.Fatalf("prose held back: %q", out)
	}
	if tail := r.Flush(); tail != "" {
		t.Fatalf("unexpected tail: %q", tail)
	}
}

func TestStreamRestorerEmitsEagerly(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	mintToken(t, s, githubKey)

	r := s.NewStreamRestorer()
	// Text with no possible token prefix at the boundary must be emitted
	// immediately, not buffered until Flush.
	if out := r.Feed("hello world. "); out != "hello world. " {
		t.Fatalf("eager emit failed: %q", out)
	}
	// A dangling "g" could start "ghp_…": exactly that suffix is held.
	if out := r.Feed("token: g"); out != "token: " {
		t.Fatalf("hold-back wrong: %q", out)
	}
	if out := r.Flush(); out != "g" {
		t.Fatalf("flush = %q, want g", out)
	}
}

func TestStreamRestorerMidStreamMint(t *testing.T) {
	// Tokens minted AFTER the restorer was created are still restorable:
	// candidates are read live from the session.
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	r := s.NewStreamRestorer()
	if out := r.Feed("start "); out != "start " {
		t.Fatalf("pre-mint feed: %q", out)
	}
	tok := mintToken(t, s, stripeKey)
	got := r.Feed(tok) + r.Flush()
	if got != stripeKey {
		t.Fatalf("mid-stream mint restore: %q", got)
	}
}
