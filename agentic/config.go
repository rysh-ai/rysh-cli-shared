// Package agentic implements the agentic coding assistant actors for use in
// both the CLI and server contexts.
package agentic

// AgentConfig holds the configuration needed to run an LLMPromptExecutionActor.
// Both the CLI (rysh-cli) and the server (rysh-server) populate this struct
// from their respective configuration systems before passing it to
// NewLLMPromptExecutionActor. This keeps the shared package free of any host-specific
// configuration dependency.
type AgentConfig struct {
	// APIKey is the Anthropic API key.
	APIKey string

	// APIURL is the base URL for the Anthropic Messages API.
	// Defaults to "https://api.anthropic.com" if empty.
	APIURL string

	// DefaultModel is the Claude model name to use.
	// Defaults to "claude-sonnet-4-20250514" if empty.
	DefaultModel string

	// MaxTokens is the maximum number of tokens per response.
	// Defaults to 4096 if zero.
	MaxTokens int

	// SystemPrompt is the system prompt text (or file path) for the agent.
	SystemPrompt string

	// BraveAPIKey is the API key for the Brave Search tool.
	BraveAPIKey string

	// MaxIterations is the maximum number of orchestrator loop iterations.
	// Defaults to 20 if zero.
	MaxIterations int
}
