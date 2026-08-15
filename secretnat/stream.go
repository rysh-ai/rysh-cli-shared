// SPDX-License-Identifier: Apache-2.0

package secretnat

import "strings"

// StreamRestorer restores synthetic tokens in a streamed text where a token
// may straddle chunk boundaries. It emits text as soon as it provably cannot
// be the beginning of a restorable token, holding back at most one
// max-token-length tail — O(1) memory, imperceptible latency.
//
// Usage: out := r.Feed(delta) per chunk, then out := r.Flush() at stream end
// (message_stop) to drain the held-back tail.
type StreamRestorer struct {
	restore    func(string) string // full restore (table tokens + ${NAME})
	candidates func() []string     // current restorable tokens (table grows mid-stream)
	tail       string
}

// NewStreamRestorer builds a restorer from a restore function and a token
// candidate provider. Both must be safe for concurrent use with the
// underlying session.
func NewStreamRestorer(restore func(string) string, candidates func() []string) *StreamRestorer {
	return &StreamRestorer{restore: restore, candidates: candidates}
}

// Feed appends chunk to the pending text and returns the longest prefix that
// is safe to emit, restored. The suffix that could still be the start of a
// token is held back for the next Feed/Flush.
func (r *StreamRestorer) Feed(chunk string) string {
	s := r.tail + chunk
	if s == "" {
		return ""
	}
	cands := r.candidates()
	hold := holdbackLen(s, cands)
	emit := s[:len(s)-hold]
	r.tail = s[len(s)-hold:]
	if emit == "" {
		return ""
	}
	return r.restore(emit)
}

// Flush drains and restores whatever is still held back. Call exactly once,
// after the final chunk.
func (r *StreamRestorer) Flush() string {
	if r.tail == "" {
		return ""
	}
	out := r.restore(r.tail)
	r.tail = ""
	return out
}

// holdbackLen returns the length of the longest suffix of s that is a proper
// prefix of any candidate token. Complete tokens inside s are NOT held (the
// restore call handles them); only genuinely ambiguous partials are.
func holdbackLen(s string, candidates []string) int {
	if len(candidates) == 0 {
		return 0
	}
	maxLen := 0
	for _, c := range candidates {
		if len(c) > maxLen {
			maxLen = len(c)
		}
	}
	// Scan from the earliest position that could start a still-incomplete
	// token; the first hit is the longest ambiguous suffix.
	start := len(s) - maxLen + 1
	if start < 0 {
		start = 0
	}
	for p := start; p < len(s); p++ {
		suffix := s[p:]
		for _, c := range candidates {
			// A proper prefix only: a fully-present token needs no holding.
			if len(suffix) < len(c) && strings.HasPrefix(c, suffix) {
				return len(s) - p
			}
		}
	}
	return 0
}
