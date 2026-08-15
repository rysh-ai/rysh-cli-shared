// SPDX-License-Identifier: Apache-2.0

// Package secretnat implements SecretNAT — also known by its alias ReSet
// (Reversible Secret Translation) — a transparent, reversible secret
// translation layer that sits between rysh and the LLM provider.
//
// Outbound text (prompts, conversation history, tool inputs/outputs, system
// prompts) is scanned for secrets; every real secret is replaced with a
// format-preserving synthetic token (e.g. "sk_live_SNAT000001" or "${NAME}").
// Inbound tokens are mapped back to real values only where a real value is
// required locally (tool execution). The LLM provider never sees a real
// secret, and the mapping table lives exclusively in memory — it is never
// persisted, logged, or exported.
//
// Two tiers of secrets exist:
//
//   - Known tier: secrets registered in the host's secret store are replaced
//     with stable "${NAME}" tokens (the same grammar as the store's Expand),
//     restorable across process restarts.
//   - Detected tier: pattern-matched secrets get "<prefix>SNAT<%06d>"
//     synthetic values whose mapping dies with the process.
package secretnat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Match is one detected secret span within a scanned text.
type Match struct {
	// Start/End delimit the secret VALUE itself (not the surrounding
	// context a detector may have matched, e.g. the "PASSWORD=" key of an
	// env-style line or the "user:" part of a database URL).
	Start, End int
	// Type is the detector name that produced this match (e.g. "stripe").
	Type string
	// Confidence in [0,1]; when overlapping matches conflict, the leftmost-
	// longest span wins first, then higher confidence.
	Confidence float64
	// Prefix is the format-preserving prefix carried into the synthetic
	// token in semantic mode (e.g. "sk_live_", "ghp_"). Empty when the
	// secret has no meaningful prefix (passwords, env values).
	Prefix string
	// Synthetic optionally overrides synthetic-token generation for this
	// match (used e.g. by the JWT detector to preserve the three-segment
	// shape). When nil the Generator's default format applies.
	Synthetic func(seq int) string
}

// Detector is the plugin interface for secret detection. Implementations
// must be safe for concurrent use and should detect in linear time (RE2
// regexps satisfy this).
type Detector interface {
	// Name identifies the detector (stable, lowercase, e.g. "github").
	Name() string
	// Detect returns all secret spans in text. Spans must not overlap
	// within a single detector's result.
	Detect(text string) []Match
	// SyntheticPrefix is the default format-preserving prefix for tokens
	// minted from this detector's matches ("" when not applicable).
	SyntheticPrefix() string
	// Validate post-filters a candidate secret value (entropy / length /
	// shape checks). Candidates failing Validate are not translated.
	Validate(candidate string) bool
}

// CustomDetector is the user-facing config shape for plugging in an extra
// regex detector without writing code (config: snat.custom_detectors).
type CustomDetector struct {
	Name    string `yaml:"name" json:"name"`
	Pattern string `yaml:"pattern" json:"pattern"`
	// Prefix is used for the synthetic token in semantic mode.
	Prefix string `yaml:"prefix" json:"prefix"`
}

// Registry holds an ordered, fixed set of detectors. Order is significant:
// it is the deterministic tiebreak for equal-length overlapping matches, so
// a Registry must not be mutated after construction.
type Registry struct {
	detectors []Detector
}

// NewRegistry builds a registry from an explicit detector list (primarily
// for tests and embedders). Most callers want NewDefaultRegistry.
func NewRegistry(detectors ...Detector) *Registry {
	return &Registry{detectors: detectors}
}

// NewDefaultRegistry returns the built-in detector set, minus any names in
// disabled, plus compiled custom detectors. Invalid custom patterns are
// reported as an error rather than silently dropped.
func NewDefaultRegistry(disabled []string, custom []CustomDetector) (*Registry, error) {
	off := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		off[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var ds []Detector
	for _, d := range builtinDetectors() {
		if !off[d.Name()] {
			ds = append(ds, d)
		}
	}
	for _, c := range custom {
		name := strings.ToLower(strings.TrimSpace(c.Name))
		if name == "" || off[name] {
			continue
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return nil, fmt.Errorf("secretnat: custom detector %q: invalid pattern: %w", c.Name, err)
		}
		ds = append(ds, &regexDetector{
			name:       name,
			re:         re,
			prefix:     c.Prefix,
			confidence: 0.7,
		})
	}
	return &Registry{detectors: ds}, nil
}

// Names returns the detector names in registry order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.detectors))
	for _, d := range r.detectors {
		out = append(out, d.Name())
	}
	return out
}

// DetectAll runs every detector over text and merges the results using
// leftmost-longest-wins overlap resolution (equal spans: higher confidence,
// then registry order). The returned matches are sorted by Start and are
// pairwise non-overlapping.
func (r *Registry) DetectAll(text string) []Match {
	if text == "" {
		return nil
	}
	type ordered struct {
		Match
		order int // registry position, deterministic tiebreak
	}
	var all []ordered
	for i, d := range r.detectors {
		for _, m := range d.Detect(text) {
			if m.Start < 0 || m.End > len(text) || m.Start >= m.End {
				continue
			}
			if !d.Validate(text[m.Start:m.End]) {
				continue
			}
			all = append(all, ordered{Match: m, order: i})
		}
	}
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Start != b.Start {
			return a.Start < b.Start // leftmost first
		}
		if a.End != b.End {
			return a.End > b.End // longest first
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.order < b.order
	})
	out := make([]Match, 0, len(all))
	lastEnd := -1
	for _, m := range all {
		if m.Start < lastEnd {
			continue // overlaps a previously accepted (leftmost-longest) span
		}
		out = append(out, m.Match)
		lastEnd = m.End
	}
	return out
}

// ---------------------------------------------------------------------------
// regexDetector — the shared implementation behind all built-ins
// ---------------------------------------------------------------------------

// regexDetector detects via a single RE2 pattern. When group > 0, the secret
// span is that capture group; otherwise the whole match. When prefixGroup >
// 0, the token prefix is taken from that capture group (preserving e.g.
// "sk_live_" vs "pk_test_"); otherwise the static prefix is used.
type regexDetector struct {
	name        string
	re          *regexp.Regexp
	group       int // secret span capture group (0 = whole match)
	prefixGroup int // prefix capture group (0 = use static prefix)
	prefix      string
	confidence  float64
	minLen      int // additional minimum secret length (0 = pattern-enforced)
	validate    func(string) bool
	synthetic   func(seq int) string
}

func (d *regexDetector) Name() string            { return d.name }
func (d *regexDetector) SyntheticPrefix() string { return d.prefix }

func (d *regexDetector) Validate(candidate string) bool {
	if d.minLen > 0 && len(candidate) < d.minLen {
		return false
	}
	// Never translate values that are already tokens or references: minted
	// synthetic tokens (idempotence), "${NAME}" references, and obvious
	// placeholders like <your-key-here>.
	if strings.Contains(candidate, "SNAT") || strings.Contains(candidate, "SECRET_TOKEN_") {
		return false
	}
	if strings.HasPrefix(candidate, "${") || strings.HasPrefix(candidate, "<") {
		return false
	}
	if d.validate != nil {
		return d.validate(candidate)
	}
	return true
}

func (d *regexDetector) Detect(text string) []Match {
	idx := d.re.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return nil
	}
	out := make([]Match, 0, len(idx))
	for _, loc := range idx {
		start, end := loc[0], loc[1]
		if d.group > 0 && 2*d.group+1 < len(loc) && loc[2*d.group] >= 0 {
			start, end = loc[2*d.group], loc[2*d.group+1]
		}
		if start < 0 || end <= start {
			continue
		}
		prefix := d.prefix
		if d.prefixGroup > 0 && 2*d.prefixGroup+1 < len(loc) && loc[2*d.prefixGroup] >= 0 {
			prefix = text[loc[2*d.prefixGroup]:loc[2*d.prefixGroup+1]]
		}
		out = append(out, Match{
			Start:      start,
			End:        end,
			Type:       d.name,
			Confidence: d.confidence,
			Prefix:     prefix,
			Synthetic:  d.synthetic,
		})
	}
	return out
}
