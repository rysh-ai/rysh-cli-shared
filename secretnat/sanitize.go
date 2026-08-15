// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// knownRefPattern matches "${NAME}" references — deliberately identical to
// the rysh secret store's expansion grammar (secretRefPattern), so known-tier
// tokens restore with the exact semantics users already have from ##secret.
var knownRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// minKnownValueLen guards against catastrophic replacements when a registered
// secret has a trivially short value (e.g. "1234" would riddle ordinary text
// with tokens).
const minKnownValueLen = 6

// KnownSecret is one (name, value) pair from the host's secret store.
type KnownSecret struct {
	Name  string
	Value string
}

// KnownSet is an immutable snapshot of the known-tier secrets. Rebuild (via
// NewKnownSet) and atomically swap on every secret-store mutation.
type KnownSet struct {
	// byLen holds the secrets sorted by value length descending, so
	// replacement is longest-value-first and a secret that is a substring
	// of another can never cause a partial replacement.
	byLen []KnownSecret
	// names resolves ${NAME} back to the real value on restore.
	names map[string]string
	// maxTokenLen bounds streaming hold-back for "${NAME}" tokens.
	maxTokenLen int
}

// NewKnownSet builds a snapshot from the given pairs, dropping entries with
// empty names or values too short to translate safely.
func NewKnownSet(secrets []KnownSecret) *KnownSet {
	ks := &KnownSet{names: make(map[string]string, len(secrets))}
	for _, s := range secrets {
		if s.Name == "" || len(s.Value) < minKnownValueLen {
			continue
		}
		if _, dup := ks.names[s.Name]; dup {
			continue
		}
		ks.names[s.Name] = s.Value
		ks.byLen = append(ks.byLen, s)
		if l := len("${" + s.Name + "}"); l > ks.maxTokenLen {
			ks.maxTokenLen = l
		}
	}
	sort.SliceStable(ks.byLen, func(i, j int) bool {
		if len(ks.byLen[i].Value) != len(ks.byLen[j].Value) {
			return len(ks.byLen[i].Value) > len(ks.byLen[j].Value)
		}
		return ks.byLen[i].Name < ks.byLen[j].Name
	})
	return ks
}

// Size returns the number of usable known secrets.
func (ks *KnownSet) Size() int {
	if ks == nil {
		return 0
	}
	return len(ks.byLen)
}

// ValueFor returns the real value registered under name, or ("", false).
// Used only by the explicit, local-only "##snat get ${NAME}" reveal.
func (ks *KnownSet) ValueFor(name string) (string, bool) {
	if ks == nil {
		return "", false
	}
	v, ok := ks.names[name]
	return v, ok
}

// Tokens returns the "${NAME}" token for every known secret.
func (ks *KnownSet) Tokens() []string {
	if ks == nil {
		return nil
	}
	out := make([]string, 0, len(ks.byLen))
	for _, s := range ks.byLen {
		out = append(out, "${"+s.Name+"}")
	}
	return out
}

// Sanitize translates every secret in text into its synthetic token:
//
//	pass 1 — known tier: registered secret values → "${NAME}", longest
//	         value first;
//	pass 2 — detected tier: registry matches → minted tokens from the
//	         mapping table (deterministic per value).
//
// Sanitize is idempotent: tokens produced by either pass are never
// re-translated (Sanitize(Sanitize(x)) == Sanitize(x)), which also keeps
// repeated sanitization byte-stable for prompt caching. Returns the
// sanitized text and the number of replacements.
func Sanitize(text string, known *KnownSet, reg *Registry, table *MappingTable, gen *Generator) (string, int) {
	if text == "" {
		return text, 0
	}
	n := 0
	if known != nil {
		for _, s := range known.byLen {
			c := strings.Count(text, s.Value)
			if c == 0 {
				continue
			}
			text = strings.ReplaceAll(text, s.Value, "${"+s.Name+"}")
			n += c
		}
	}
	if reg == nil || table == nil || gen == nil {
		return text, n
	}
	matches := reg.DetectAll(text)
	if len(matches) == 0 {
		return text, n
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		candidate := text[m.Start:m.End]
		// Idempotence: never re-translate our own tokens or known refs.
		if table.IsToken(candidate) || strings.Contains(candidate, "${") {
			continue
		}
		b.WriteString(text[last:m.Start])
		mm := m
		b.WriteString(table.TokenFor(candidate, m.Type, func(seq int) string {
			return gen.Synthetic(mm, seq)
		}))
		last = m.End
		n++
	}
	b.WriteString(text[last:])
	return b.String(), n
}

// Restore replaces synthetic tokens in text with real values:
//
//   - minted detected-tier tokens (exact table hits, longest first);
//   - "${NAME}" references whose NAME exists in the known set — a
//     model-written "${HOME}" stays untouched unless HOME is actually a
//     registered secret.
//
// Returns the restored text and the number of replacements.
func Restore(text string, known *KnownSet, table *MappingTable) (string, int) {
	if text == "" {
		return text, 0
	}
	n := 0
	if table != nil {
		var c int
		text, c = table.RestoreAll(text)
		n += c
	}
	if known != nil && len(known.names) > 0 && strings.Contains(text, "${") {
		text = knownRefPattern.ReplaceAllStringFunc(text, func(ref string) string {
			name := ref[2 : len(ref)-1]
			if v, ok := known.names[name]; ok {
				n++
				return v
			}
			return ref
		})
	}
	return text, n
}

// transformJSON applies a string transform to every string leaf of a JSON
// document, preserving numbers exactly (json.Number) and re-encoding
// deterministically (Go sorts object keys). Walking the decoded value —
// instead of substring surgery on raw bytes — keeps JSON escaping correct
// when secret values contain quotes, backslashes, or newlines. On malformed
// input the raw bytes are returned unchanged. Session.SanitizeJSON and
// Session.RestoreJSON are built on this.
func transformJSON(raw json.RawMessage, fn func(string) string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	v = walkJSON(v, fn)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func walkJSON(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		for k, val := range t {
			t[k] = walkJSON(val, fn)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = walkJSON(val, fn)
		}
		return t
	default:
		return v
	}
}
