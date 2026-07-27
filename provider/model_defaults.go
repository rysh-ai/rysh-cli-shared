package provider

import "strings"

// DefaultMaxTokensForModel returns a sensible per-model max_tokens budget.
// Used when the caller passes maxTokens <= 0 to NewClaudeAgenticProvider.
//
// Rationale: current-generation models (Opus 4.6+, Sonnet 4.6+/5, Fable 5)
// support up to 128k output tokens, but very large non-streaming responses
// risk HTTP timeouts, so 16k is the safe ceiling that also stops truncating
// long edits / refactors (the old 4096 Opus default cut them off mid-file).
// Haiku models get a smaller budget to keep otherwise-cheap calls cheap.
// Unknown models fall back to 8192 — conservative but rarely truncating.
func DefaultMaxTokensForModel(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		return 16000
	case strings.Contains(m, "opus"):
		return 16000
	case strings.Contains(m, "sonnet"):
		return 16000
	case strings.Contains(m, "haiku"):
		return 8192
	}
	return 8192
}
