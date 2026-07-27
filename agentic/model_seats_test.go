package agentic

import (
	"context"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// seatProv is a fake ModelEffortOverridable provider recording overrides.
type seatProv struct {
	provider.AgenticProvider
	model, effort string
}

func (s *seatProv) WithModelEffort(model, effort string) provider.AgenticProvider {
	cp := *s
	if model != "" {
		cp.model = model
	}
	if effort != "" {
		cp.effort = effort
	}
	return &cp
}
func (s *seatProv) Name() string { return "seat-fake" }
func (s *seatProv) Complete(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (s *seatProv) CompleteWithTools(_ context.Context, _ []provider.ConversationTurn, _ []provider.ToolSpec, _ string) (*provider.AgenticResponse, error) {
	return &provider.AgenticResponse{}, nil
}

// TestLegProviderSeats covers per-leg seat selection: main legs use the run
// seat, the finalizer leg uses the finalizer seat with fallback to the run
// seat, and no seats → the configured provider unchanged.
func TestLegProviderSeats(t *testing.T) {
	base := &seatProv{}
	a := &LLMPromptExecutionActor{prov: base}

	// No seats → same provider instance.
	if got := a.legProvider(); got != provider.AgenticProvider(base) {
		t.Error("no seats should return the configured provider unchanged")
	}

	// Run seat on a main leg.
	a.runModel, a.runEffort = "claude-sonnet-5", "high"
	got := a.legProvider().(*seatProv)
	if got.model != "claude-sonnet-5" || got.effort != "high" {
		t.Errorf("run seat not applied: %+v", got)
	}

	// Finalizer leg falls back to the run seat when its own seat is empty.
	a.finalizerPhase = true
	got = a.legProvider().(*seatProv)
	if got.model != "claude-sonnet-5" || got.effort != "high" {
		t.Errorf("finalizer fallback to run seat failed: %+v", got)
	}

	// Finalizer seat wins on the finalizer leg.
	a.runFinalizerModel, a.runFinalizerEffort = "claude-haiku-4-5", "low"
	got = a.legProvider().(*seatProv)
	if got.model != "claude-haiku-4-5" || got.effort != "low" {
		t.Errorf("finalizer seat not applied: %+v", got)
	}

	// Back on a main leg the run seat still applies.
	a.finalizerPhase = false
	got = a.legProvider().(*seatProv)
	if got.model != "claude-sonnet-5" || got.effort != "high" {
		t.Errorf("main leg should use the run seat: %+v", got)
	}
}

// (TestPruneScreenshotBlocks was removed with pruneScreenshotBlocks itself:
// screenshots are no longer stored in the transcript — mutating earlier turns
// invalidated the incremental prompt cache every round. The replacement
// behavior is covered by screenshot_ephemeral_test.go: the stored transcript
// stays append-only and image-free; the current capture rides each request as
// an ephemeral trailing turn.)
