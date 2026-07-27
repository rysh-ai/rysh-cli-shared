package agentic

import (
	"fmt"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

func turnsN(n int) []provider.ConversationTurn {
	out := make([]provider.ConversationTurn, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, provider.ConversationTurn{Role: "user", Content: fmt.Sprintf("turn %d", i)})
	}
	return out
}

// Hysteresis: below the hard threshold the window must NOT slide — a slide
// changes the transcript head, which invalidates the incremental prompt cache
// and re-writes the whole conversation at full budget weight every leg.
func TestBoundConversation_HysteresisNoSlideBelowHardLimit(t *testing.T) {
	for _, n := range []int{10, maxConversationTurns, maxConversationTurns + 50, maxConversationTurnsHard} {
		got := boundConversation(turnsN(n))
		if len(got) != n {
			t.Fatalf("n=%d: trimmed to %d — must not slide below the hard limit", n, len(got))
		}
		if got[0].Content != "turn 0" {
			t.Fatalf("n=%d: head moved without exceeding the hard limit", n)
		}
	}
}

func TestBoundConversation_TrimsToTargetPastHardLimit(t *testing.T) {
	n := maxConversationTurnsHard + 1
	got := boundConversation(turnsN(n))
	if len(got) != maxConversationTurns {
		t.Fatalf("want trim to target %d, got %d", maxConversationTurns, len(got))
	}
	// The head is now stable for the NEXT ~(hard-target) turns: growing the
	// input by a few turns after a trim must not move the head again.
	grown := append(got, turnsN(5)...)
	again := boundConversation(grown)
	if again[0].Content != got[0].Content {
		t.Fatal("head moved again immediately after a trim — hysteresis broken")
	}
}
