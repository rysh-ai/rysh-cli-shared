// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"strings"
	"testing"
)

// rulingCitation is the marker every user-facing client-streaming/bidi refusal
// must carry. T3 ruled those methods a PERMANENT non-goal with written reasons;
// an error that only states the mechanism ("cannot ride a JSON-over-HTTP
// bridge") reads like a limitation somebody will lift next week, and gives a
// user no way to discover that it is a decision or what would reopen it.
const rulingCitation = "design 015 §5.1"

// TestGRPCAllClientBidiErrorCitesTheRuling is T3's error-text half at the
// ingest site. This is the total-refusal path: every method in the descriptor
// set is client-streaming or bidi, so nothing is callable and ingest fails
// outright.
//
// Both gRPC ingest paths reach this one error. GRPCReflect (--grpc-target)
// returns "the exact input the file-based GRPC ingester consumes" — the wire
// bytes of a FileDescriptorSet — and callers flow it through GRPC() unchanged
// (forgecmd.ingestSpec, forge/manager.go). So there is a single message here,
// not one per path, and this test covers both.
func TestGRPCAllClientBidiErrorCitesTheRuling(t *testing.T) {
	_, err := GRPC(onlyStreamDescriptorSet(t, true))
	if err == nil {
		t.Fatal("GRPC() with only client/bidi methods returned nil error, want an error")
	}
	got := err.Error()

	if !strings.Contains(got, rulingCitation) {
		t.Errorf("refusal does not cite where the reasons live (want %q):\n  %s", rulingCitation, got)
	}
	if !strings.Contains(got, "ruling") {
		t.Errorf("refusal does not name itself a ruling, so it reads as a gap awaiting work:\n  %s", got)
	}
	// The mechanism is still worth stating; it just must not be the whole
	// story. Keep the factual part that tells the user WHAT was refused.
	if !strings.Contains(got, "client-streaming") && !strings.Contains(got, "bidi") {
		t.Errorf("refusal no longer says which kinds were refused:\n  %s", got)
	}
}

// TestGRPCPartialSkipStillIngests guards the boundary the ruling must not move:
// a service with SOME callable methods still ingests. T3 changes error text
// only — it must not turn a partial skip into a refusal.
func TestGRPCPartialSkipStillIngests(t *testing.T) {
	api, err := GRPC(buildStreamingFileDescriptorSet(t))
	if err != nil {
		t.Fatalf("GRPC() with a mix of unary/streaming/bidi must succeed, got: %v", err)
	}
	if len(api.Skipped) == 0 {
		t.Fatal("fixture is malformed: expected at least one skipped client/bidi method")
	}
	if len(api.Operations) == 0 && len(api.Streams) == 0 {
		t.Error("nothing callable was produced, so this no longer exercises the partial-skip path")
	}
}
