package provider

// Phase 3 item F — extended thinking.
//
// Anthropic's "extended thinking" feature lets the model emit hidden reasoning
// blocks before its visible answer. The model receives a thinking-token
// budget on each request; the response carries one or more `thinking`
// content blocks alongside text / tool_use blocks. Streaming surfaces them
// as `thinking_delta` events on a dedicated content_block index.
//
// This package adds:
//   - a ThinkingConfig type sent on agenticRequest.Thinking
//   - a ThinkingBlock type in AgenticResponse
//   - streaming-event types for thinking deltas (in streaming.go)
//
// Hooks consumed by the orchestrator are in rysh-shared/agentic — the
// orchestrator emits thinking through a dedicated "thinking" output type so
// the UI can render it dimmed / collapsed.

// ThinkingConfig is sent as `request.thinking` to enable extended thinking.
// Two modes:
//
//	{"type": "adaptive"}                          — Claude 4.6+ models (recommended);
//	                                                the model decides when/how much to think.
//	{"type": "enabled", "budget_tokens": 8192}    — legacy pre-4.6 models only. This
//	                                                form is REJECTED with a 400 on
//	                                                Opus 4.7+ / Sonnet 5 — do not use
//	                                                it on current models.
//
// Use AdaptiveThinkingConfig() for current models; LegacyThinkingConfig(n)
// only when explicitly targeting a pre-4.6 model.
type ThinkingConfig struct {
	// Type is "adaptive" (recommended, 4.6+ models) or "enabled" (legacy).
	Type string `json:"type"`

	// BudgetTokens is the legacy fixed thinking budget. Only set with
	// Type == "enabled" on pre-4.6 models; omitted from the wire form
	// otherwise (adaptive mode has no budget field).
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// AdaptiveThinkingConfig returns the adaptive thinking config recommended for
// Claude 4.6+ models: the model dynamically decides when and how much to
// think, and interleaved thinking between tool calls is enabled automatically.
func AdaptiveThinkingConfig() *ThinkingConfig {
	return &ThinkingConfig{Type: "adaptive"}
}

// LegacyThinkingConfig returns the pre-4.6 fixed-budget thinking config.
// Rejected with a 400 by Opus 4.7+ / Sonnet 5 — use AdaptiveThinkingConfig
// unless explicitly targeting an older model.
func LegacyThinkingConfig(budgetTokens int) *ThinkingConfig {
	if budgetTokens <= 0 {
		budgetTokens = 8192
	}
	return &ThinkingConfig{Type: "enabled", BudgetTokens: budgetTokens}
}

// DefaultThinkingConfig returns the recommended thinking config for current
// models. Adaptive since the Claude 4.6 family; the old fixed-budget form is
// available via LegacyThinkingConfig for pre-4.6 models.
func DefaultThinkingConfig() *ThinkingConfig {
	return AdaptiveThinkingConfig()
}

// ThinkingBlock is a hidden-reasoning content block in the model response.
// Carries the raw thinking text plus an opaque signature the API uses to
// verify thinking provenance across multi-turn exchanges. The signature
// MUST be echoed verbatim if a subsequent turn needs to reference the same
// thinking block.
type ThinkingBlock struct {
	Text      string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}
