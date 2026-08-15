// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// MappingEntry is the value-free view of one translation, safe to display
// (##snat list) and log: it carries the synthetic token and metadata but
// NEVER the real value.
type MappingEntry struct {
	Token    string
	Detector string
	Hits     int
}

// mappingRecord is the internal, value-bearing record. It never leaves the
// table.
type mappingRecord struct {
	value    string
	token    string
	detector string
	hits     int
}

// MappingTable is the in-memory, per-conversation bidirectional map between
// real secret values and synthetic tokens.
//
// Security invariants:
//   - lives only in memory; MarshalJSON fails loudly so the table can never
//     ride along an accidentally-serialized struct into KV or logs;
//   - real values are reachable only via RestoreAll / lookups, never via
//     Entries()/stats.
type MappingTable struct {
	mu          sync.RWMutex
	valueToRec  map[string]*mappingRecord
	tokenToRec  map[string]*mappingRecord
	seq         int
	restored    int
	lastUsed    time.Time
	maxTokenLen int
}

// NewMappingTable returns an empty table.
func NewMappingTable() *MappingTable {
	return &MappingTable{
		valueToRec: make(map[string]*mappingRecord),
		tokenToRec: make(map[string]*mappingRecord),
		lastUsed:   time.Now(),
	}
}

// MarshalJSON always fails: the mapping table must never be serialized. A
// loud error beats a silent secret leak into JetStream KV or a log line.
func (t *MappingTable) MarshalJSON() ([]byte, error) {
	return nil, errors.New("secretnat: MappingTable must never be serialized")
}

// TokenFor returns the synthetic token for value, minting one via mint on
// first sight. Deterministic within a table: the same value always yields
// the same token. mint receives the next sequence number.
func (t *MappingTable) TokenFor(value, detector string, mint func(seq int) string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastUsed = time.Now()
	if rec, ok := t.valueToRec[value]; ok {
		rec.hits++
		return rec.token
	}
	t.seq++
	token := mint(t.seq)
	rec := &mappingRecord{value: value, token: token, detector: detector, hits: 1}
	t.valueToRec[value] = rec
	t.tokenToRec[token] = rec
	if len(token) > t.maxTokenLen {
		t.maxTokenLen = len(token)
	}
	return token
}

// IsToken reports whether s is a token minted by this table.
func (t *MappingTable) IsToken(s string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.tokenToRec[s]
	return ok
}

// RevealToken returns the real value mapped to a detected-tier token, or
// ("", false). This is the ONLY value-returning accessor; it exists for the
// explicit, local-only "##snat get" reveal and must never be used on any
// outbound / persistence path.
func (t *MappingTable) RevealToken(token string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if rec, ok := t.tokenToRec[token]; ok {
		return rec.value, true
	}
	return "", false
}

// RestoreAll replaces every minted token in text with its real value,
// longest-token-first so no token that is a substring of another can cause a
// partial replacement. Returns the restored text and the number of
// replacements made.
func (t *MappingTable) RestoreAll(text string) (string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.tokenToRec) == 0 || text == "" {
		return text, 0
	}
	t.lastUsed = time.Now()
	tokens := make([]string, 0, len(t.tokenToRec))
	for tok := range t.tokenToRec {
		tokens = append(tokens, tok)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if len(tokens[i]) != len(tokens[j]) {
			return len(tokens[i]) > len(tokens[j])
		}
		return tokens[i] < tokens[j]
	})
	n := 0
	for _, tok := range tokens {
		c := countNonOverlapping(text, tok)
		if c == 0 {
			continue
		}
		text = strings.ReplaceAll(text, tok, t.tokenToRec[tok].value)
		n += c
	}
	t.restored += n
	return text, n
}

// Entries returns the value-free listing for display, ordered by token.
func (t *MappingTable) Entries() []MappingEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]MappingEntry, 0, len(t.tokenToRec))
	for _, rec := range t.tokenToRec {
		out = append(out, MappingEntry{Token: rec.token, Detector: rec.detector, Hits: rec.hits})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

// PerDetector returns detection hit counts keyed by detector name.
func (t *MappingTable) PerDetector() map[string]int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]int)
	for _, rec := range t.tokenToRec {
		out[rec.detector] += rec.hits
	}
	return out
}

// RestoredCount returns the number of token→value replacements performed.
func (t *MappingTable) RestoredCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.restored
}

// Size returns the number of mappings.
func (t *MappingTable) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.tokenToRec)
}

// MaxTokenLen returns the longest minted token's length (streaming restorers
// use it to bound their hold-back window).
func (t *MappingTable) MaxTokenLen() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.maxTokenLen
}

// Tokens returns all minted tokens (no values), longest first.
func (t *MappingTable) Tokens() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.tokenToRec))
	for tok := range t.tokenToRec {
		out = append(out, tok)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// LastUsed returns the last time the table translated or restored anything.
func (t *MappingTable) LastUsed() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastUsed
}

// countNonOverlapping counts non-overlapping occurrences of sub in s.
func countNonOverlapping(s, sub string) int {
	if sub == "" {
		return 0
	}
	return strings.Count(s, sub)
}
