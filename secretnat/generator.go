// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"fmt"
	"strings"
)

// Mode selects the synthetic-token style.
type Mode string

const (
	// ModeSemantic preserves the credential's visible type so the LLM keeps
	// full context: "sk_live_SNAT000042", "ghp_SNAT000007".
	ModeSemantic Mode = "semantic"
	// ModePrivate hides even the provider/type: "SECRET_TOKEN_042". Better
	// privacy, less context for the LLM.
	ModePrivate Mode = "private"
)

// ParseMode normalizes a config string into a Mode, defaulting to semantic.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(ModePrivate), "high-privacy", "high_privacy":
		return ModePrivate
	default:
		return ModeSemantic
	}
}

// Generator mints synthetic tokens for detected-tier secrets. Tokens embed
// the fixed "SNAT" marker plus a fixed-width sequence number: distinctive
// enough that no built-in detector re-matches its own output (idempotent
// sanitize) and no real-world text collides with it.
type Generator struct {
	mode Mode
}

// NewGenerator returns a generator for the given mode.
func NewGenerator(mode Mode) *Generator {
	if mode != ModePrivate {
		mode = ModeSemantic
	}
	return &Generator{mode: mode}
}

// Mode returns the generator's mode.
func (g *Generator) Mode() Mode { return g.mode }

// Synthetic mints the token for sequence number seq from a match's shape.
// In semantic mode the match's own Synthetic template wins (e.g. JWT's
// three-segment shape), then its format-preserving prefix, then a
// detector-name fallback. Private mode always yields SECRET_TOKEN_<n>.
func (g *Generator) Synthetic(m Match, seq int) string {
	if g.mode == ModePrivate {
		return fmt.Sprintf("SECRET_TOKEN_%03d", seq)
	}
	if m.Synthetic != nil {
		return m.Synthetic(seq)
	}
	if m.Prefix != "" {
		return fmt.Sprintf("%sSNAT%06d", m.Prefix, seq)
	}
	name := strings.ToUpper(strings.ReplaceAll(m.Type, "-", "_"))
	if name == "" {
		name = "SECRET"
	}
	return fmt.Sprintf("%s_SNAT_%06d", name, seq)
}
