// Package provider implements LLM provider interfaces and the Anthropic
// Messages API client with tool-use support.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const anthropicVersion = "2023-06-01"

// DefaultClaudeModel is the model used when the host doesn't configure one.
// claude-opus-4-8 is the current Opus-tier alias (un-dated; date-suffixed IDs
// like claude-sonnet-4-20250514 are deprecated — that one already 404s).
//
// Exported because every host used to carry its OWN copy of this string, and
// copies rot at different rates: rysh-server sat on claude-opus-4-5 long after
// the engine had moved on, and AgentConfig's doc comment still advertised a
// model the API no longer serves. Hosts that want "whatever the engine
// defaults to" must be able to SAY that rather than re-type it.
const DefaultClaudeModel = "claude-opus-4-8"

// ---------------------------------------------------------------------------
// Provider interfaces
// ---------------------------------------------------------------------------

// Provider is the basic single-turn LLM interface.
type Provider interface {
	Name() string
	Complete(ctx context.Context, prompt string) (string, error)
}

// StopReason indicates why the model stopped generating.
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonToolUse   StopReason = "tool_use"
	StopReasonMaxTokens StopReason = "max_tokens"
	StopReasonStopSeq   StopReason = "stop_sequence"
)

// Turn categories classify the origin of a conversation turn in the session
// JSON. They mirror the pane's per-mode output categorisation (shell / ai /
// rysh / chat) and extend it with agentic origins (tool / subagent). External
// renderers (TUI, Slack flow, session_history) key off these instead of
// re-deriving them from Role.
const (
	TurnCategoryUser     = "user"     // human-typed prompt
	TurnCategoryAI       = "ai"       // assistant text / tool_use turns
	TurnCategoryShell    = "shell"    // shell-derived context injected into the conversation
	TurnCategoryRysh     = "rysh"     // rysh system-command derived turns
	TurnCategoryChat     = "chat"     // chat-mode messages (agents registered to a pane)
	TurnCategoryTool     = "tool"     // tool_result turns (Origin = tool name)
	TurnCategorySubAgent = "subagent" // sub-agent summary turns (Origin = child orchestrator id)
	TurnCategorySystem   = "system"   // synthetic turns (compaction summaries, interrupts)
)

// ConversationTurn represents a single turn in a multi-turn conversation.
//
// The un-tagged fields keep their historical Go-default JSON names (Role,
// Content, …) so conversations persisted to KV before categorisation existed
// still restore. The tagged fields below are additive metadata.
type ConversationTurn struct {
	Role          string            // "user", "assistant", or "tool"
	Content       string            // text content (for user messages or assistant text)
	ContentBlocks []ContentBlock    // structured content (e.g. text + image); when non-empty for a user turn, supersedes Content. Follow-up item 1.
	ToolCalls     []ToolCallRequest // tool_use blocks produced by assistant
	ToolCallID    string            // for role "tool": the tool_use id this result corresponds to
	IsError       bool              // for role "tool": whether this result is an error

	// Category classifies the turn's origin (TurnCategory* constants above):
	// shell / ai / rysh / chat for pane-mode parity, tool for tool results,
	// subagent for sub-agent summaries, system for synthetic turns. Serialized
	// into the session JSON so downstream consumers (Slack flow, history tools)
	// respect the pane categorisation.
	Category string `json:"category,omitempty"`
	// Origin identifies the producer within the category: the tool name for
	// Category "tool", the child orchestrator id for "subagent", the pane /
	// agent / humanoid name for "shell"/"chat" injections.
	Origin string `json:"origin,omitempty"`
	// Summary is a short human-readable digest of the turn (a step title for
	// tool calls, an LLM/deterministic summary for sub-agent results). Used by
	// low-bandwidth renderers (Slack step flow) instead of full Content.
	Summary string `json:"summary,omitempty"`
	// TimestampMs records when the turn was appended (unix milliseconds).
	TimestampMs int64 `json:"timestamp_ms,omitempty"`
	// Thinking carries the assistant turn's extended-thinking blocks so they
	// can be replayed on the next request (the Messages API requires thinking
	// blocks to precede tool_use blocks in replayed assistant turns when
	// extended thinking is enabled).
	Thinking []ThinkingBlock `json:"thinking,omitempty"`

	// ProviderName / Model record WHICH model produced an assistant turn.
	//
	// A conversation outlives the model that started it: `##llm select` swaps
	// the provider on a live orchestrator, so the next model inherits a
	// transcript of assistant turns it did not write. Without attribution it
	// reads them as its own — one model answered "I'm ChatGPT", the next was
	// asked the same question, saw that answer in its own voice, and
	// "corrected" itself. Role alone cannot express this: every assistant turn
	// is role "assistant" no matter who wrote it.
	//
	// Empty on user/tool turns, and on assistant turns recorded before this
	// existed — treat empty as "unknown", never as "mine".
	ProviderName string `json:"provider_name,omitempty"`
	Model        string `json:"model,omitempty"`
}

// ProducedBy reports whether this turn was written by the given provider/model
// pair. An unattributed turn belongs to nobody: it answers false for every
// model, which is what keeps a legacy transcript from being claimed.
func (t ConversationTurn) ProducedBy(providerName, model string) bool {
	if t.ProviderName == "" && t.Model == "" {
		return false
	}
	return t.ProviderName == providerName && t.Model == model
}

// Attribution renders a turn's producer for display, e.g. "openai
// (gpt-5.6-luna)". Empty when the turn carries no attribution.
func (t ConversationTurn) Attribution() string {
	switch {
	case t.ProviderName == "" && t.Model == "":
		return ""
	case t.Model == "":
		return t.ProviderName
	case t.ProviderName == "":
		return t.Model
	}
	return t.ProviderName + " (" + t.Model + ")"
}

// CategorizeTurn returns the default category for a turn based on its role,
// used when the producer didn't set one explicitly.
func CategorizeTurn(t ConversationTurn) string {
	if t.Category != "" {
		return t.Category
	}
	switch t.Role {
	case "user":
		return TurnCategoryUser
	case "assistant":
		return TurnCategoryAI
	case "tool":
		return TurnCategoryTool
	}
	return TurnCategorySystem
}

// ToolSpec defines a tool that the model may invoke.
type ToolSpec struct {
	Name        string          // tool name
	Description string          // human-readable description
	Parameters  json.RawMessage // JSON Schema for the tool input
}

// ToolCallRequest is a single tool invocation requested by the model.
type ToolCallRequest struct {
	ID    string          // unique id assigned by the API
	Name  string          // tool name
	Input json.RawMessage // raw JSON input arguments
}

// TextBlock is a text segment in the model response.
type TextBlock struct {
	Text string
}

// AgenticResponse is the structured response from CompleteWithTools.
type AgenticResponse struct {
	TextBlocks     []TextBlock       // text content blocks
	ToolCalls      []ToolCallRequest // tool_use blocks
	ThinkingBlocks []ThinkingBlock   // extended-thinking content blocks (Phase 3 F)
	StopReason     StopReason        // why the model stopped
	Usage          Usage             // token accounting for this response
}

// Usage reports token accounting for a single API response, including
// prompt-cache hit/write counts. Callers use the input figures to drive
// context-window budgeting and compaction.
type Usage struct {
	InputTokens              int // non-cached input tokens billed at full rate
	OutputTokens             int // generated output tokens
	CacheReadInputTokens     int // input tokens served from the prompt cache
	CacheCreationInputTokens int // input tokens written to the prompt cache
}

// TotalInputTokens returns the full prompt size for this turn (uncached +
// cache-read + cache-write). This is the figure to compare against the model's
// context window when deciding whether to compact the conversation.
func (u Usage) TotalInputTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// NewTokens returns the tokens genuinely processed on this call — new input
// (uncached + newly cached) plus generated output — but EXCLUDING cache reads,
// which are the re-sent conversation prefix already counted on earlier calls.
// Summing NewTokens across calls approximates the run's unique token volume
// ("total reading"), growing ~linearly with real work; summing TotalInputTokens
// instead re-counts the whole growing conversation every call (quadratic-ish),
// which made a cumulative token budget fire far too early. Used for the run's
// token_budget accounting (NOT context-window compaction, which uses the peak
// TotalInputTokens).
func (u Usage) NewTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.OutputTokens
}

// AgenticProvider extends Provider with multi-turn tool-use capabilities.
type AgenticProvider interface {
	Provider
	CompleteWithTools(
		ctx context.Context,
		conversation []ConversationTurn,
		tools []ToolSpec,
		systemPrompt string,
	) (*AgenticResponse, error)
}

// ---------------------------------------------------------------------------
// ClaudeAgenticProvider — Anthropic Messages API with tool use
// ---------------------------------------------------------------------------

// ClaudeAgenticProvider implements AgenticProvider using the Anthropic Messages API.
type ClaudeAgenticProvider struct {
	apiKey    string
	apiURL    string
	model     string
	maxTokens int
	client    *http.Client

	// cacheEnabled toggles Anthropic prompt caching (cache_control breakpoints
	// on the system prompt, tool definitions, and the trailing conversation
	// block). On by default; the API silently ignores caching when the cached
	// prefix is below the model's minimum, so it is always safe to leave on.
	cacheEnabled bool

	// retryPolicy controls how transient API errors (429, 529, 5xx, network)
	// are retried. Phase 3 G. DefaultRetryPolicy() is applied at construction.
	retryPolicy RetryPolicy

	// modelFallback, when Secondary is non-empty, triggers a one-shot retry
	// against that model when the primary's retry budget is exhausted.
	// Phase 3 G.
	modelFallback ModelFallback

	// thinking carries the extended-thinking configuration sent on each
	// request when enabled. Phase 3 F.
	thinking *ThinkingConfig

	// effort, when set, is sent as output_config.effort on every request
	// (low|medium|high|xhigh|max). Set via WithModelEffort.
	effort string
}

// ModelEffortOverridable is implemented by providers that can produce a
// per-run variant of themselves with a different model and/or effort — the
// seam behind recipe-level `do.model`/`do.effort` (and the finalizer seat).
// Callers type-assert; providers that don't implement it simply run with
// their configured defaults.
type ModelEffortOverridable interface {
	// WithModelEffort returns a provider identical to the receiver except for
	// the non-empty overrides. The receiver is not mutated.
	WithModelEffort(model, effort string) AgenticProvider
}

// WithModelEffort returns a shallow copy of the provider with the non-empty
// model/effort overrides applied. Safe for concurrent use: the copy shares
// the HTTP client but no mutable state is written after construction.
func (c *ClaudeAgenticProvider) WithModelEffort(model, effort string) AgenticProvider {
	cp := *c
	if m := strings.TrimSpace(model); m != "" {
		cp.model = m
	}
	if e := strings.TrimSpace(effort); e != "" {
		cp.effort = e
	}
	return &cp
}

// MaxTokensOverridable is implemented by providers that can produce a
// per-request variant of themselves with a different max_tokens output cap —
// the seam behind ChatRequest.MaxTokens, mirroring ModelEffortOverridable.
// Callers type-assert; providers without the seam keep their
// construction-time cap (and the ChatProvider path errors loudly rather than
// silently dropping a requested cap).
type MaxTokensOverridable interface {
	// WithMaxTokens returns a provider identical to the receiver except for
	// its max_tokens output cap; maxTokens <= 0 keeps the receiver's cap.
	// The receiver is not mutated. Wrappers that forward the seam return nil
	// when the wrapped provider cannot honor the cap, so callers can fail
	// loudly instead of silently sending an uncapped request.
	WithMaxTokens(maxTokens int) AgenticProvider
}

// WithMaxTokens returns a shallow copy of the provider with the max_tokens
// cap applied. Safe for concurrent use for the same reason WithModelEffort
// is: the copy shares the HTTP client but no mutable state is written after
// construction.
func (c *ClaudeAgenticProvider) WithMaxTokens(maxTokens int) AgenticProvider {
	cp := *c
	if maxTokens > 0 {
		cp.maxTokens = maxTokens
	}
	return &cp
}

// IsEffortUnsupportedErr reports whether an API error says the model does
// not support the effort parameter (the shape Anthropic returns for e.g.
// Haiku 4.5). Used to self-heal seats that paired effort with such a model.
func IsEffortUnsupportedErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not support the effort parameter")
}

// IsInvalidRequestErr reports whether an API error is a deterministic
// HTTP 400 invalid_request_error (e.g. an image block whose declared
// media_type doesn't match its bytes). These are permanent for a given
// request payload: re-sending the same conversation yields the same
// rejection, so callers must fail fast instead of retrying — a retry loop
// on a 400 silently burns the run's entire budget on identical failures.
// Matches the error strings produced by completeOnce/streamOnce
// ("…: invalid_request_error (HTTP 400): …" or "…: HTTP 400: …").
func IsInvalidRequestErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_request_error") || strings.Contains(msg, "HTTP 400")
}

// outputConfig renders the request output_config block, or nil when no
// effort override is set (keeping the request body byte-identical to before).
func (c *ClaudeAgenticProvider) outputConfig() *agenticOutputConfig {
	if c.effort == "" {
		return nil
	}
	return &agenticOutputConfig{Effort: c.effort}
}

// NewClaudeAgenticProvider creates a new agentic provider with explicit parameters.
func NewClaudeAgenticProvider(apiKey, apiURL, model string, maxTokens int) *ClaudeAgenticProvider {
	if model == "" {
		model = DefaultClaudeModel
	}
	if maxTokens <= 0 {
		// Phase 3 smaller-wins: per-model defaults so long edits don't get
		// truncated on Sonnet 4.x (8k output budget) while keeping Opus /
		// Haiku at a safe 4k. Unknown models fall back to 4k.
		maxTokens = DefaultMaxTokensForModel(model)
	}
	if apiURL == "" {
		apiURL = "https://api.anthropic.com"
	}
	return &ClaudeAgenticProvider{
		apiKey:       apiKey,
		apiURL:       strings.TrimRight(apiURL, "/"),
		model:        model,
		maxTokens:    maxTokens,
		client:       &http.Client{},
		cacheEnabled: true,
		retryPolicy:  DefaultRetryPolicy(),
	}
}

// SetCacheEnabled toggles prompt caching. Caching is enabled by default.
func (c *ClaudeAgenticProvider) SetCacheEnabled(enabled bool) {
	c.cacheEnabled = enabled
}

// SetRetryPolicy installs a custom retry/backoff policy. Pass
// RetryPolicy{MaxAttempts: 1} to disable retries entirely.
func (c *ClaudeAgenticProvider) SetRetryPolicy(p RetryPolicy) {
	c.retryPolicy = p
}

// SetModelFallback installs a one-shot secondary model used when the primary
// exhausts its retry budget. Pass ModelFallback{} (empty Secondary) to
// disable the fallback. Phase 3 G.
func (c *ClaudeAgenticProvider) SetModelFallback(fb ModelFallback) {
	c.modelFallback = fb
}

// SetThinking installs the extended-thinking configuration. Pass nil to
// disable thinking. Phase 3 F.
func (c *ClaudeAgenticProvider) SetThinking(cfg *ThinkingConfig) {
	c.thinking = cfg
}

func (c *ClaudeAgenticProvider) Name() string {
	return "claude-agentic"
}

// Model returns the model this provider is configured to call. Used by the
// usage ledger (design 003) to attribute token spend to a specific model for
// pricing. Additive accessor — not part of the AgenticProvider interface, so
// consumers type-assert for it and fall back to an empty model when absent.
func (c *ClaudeAgenticProvider) Model() string {
	return c.model
}

func (c *ClaudeAgenticProvider) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := c.CompleteWithTools(ctx, []ConversationTurn{
		{Role: "user", Content: prompt},
	}, nil, "")
	if err != nil {
		return "", err
	}
	var parts []string
	for _, tb := range resp.TextBlocks {
		parts = append(parts, tb.Text)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("claude-agentic: response contained no text content")
	}
	return strings.Join(parts, "\n"), nil
}

// ---------- request types ----------

type agenticRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    interface{}      `json:"system,omitempty"` // string or []systemBlock (when caching)
	Messages  []agenticMessage `json:"messages"`
	Tools     []agenticToolDef `json:"tools,omitempty"`
	// Stream toggles SSE streaming. omitempty keeps the non-streaming path
	// byte-for-byte identical to its pre-streaming form.
	Stream bool `json:"stream,omitempty"`
	// Thinking enables extended thinking on the request. omitempty keeps the
	// request body unchanged when not in use. Phase 3 F.
	Thinking *ThinkingConfig `json:"thinking,omitempty"`
	// OutputConfig carries output-level knobs; currently only effort. omitempty
	// keeps the request body unchanged when no effort override is set.
	OutputConfig *agenticOutputConfig `json:"output_config,omitempty"`
}

// agenticOutputConfig is the Anthropic output_config request block (effort
// only for now: low|medium|high|xhigh|max).
type agenticOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type agenticMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []contentBlock
}

// cacheControl marks a content/system/tool block as a prompt-cache breakpoint.
type cacheControl struct {
	Type string `json:"type"` // always "ephemeral"
}

// ephemeralCache is the singleton breakpoint marker reused across blocks.
var ephemeralCache = &cacheControl{Type: "ephemeral"}

// systemBlock is a structured system-prompt block. The array form is required
// to attach cache_control to the system prompt.
type systemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type contentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`

	// Extended-thinking replay (Type=="thinking"). Thinking blocks recorded
	// on assistant turns are replayed with their signature so tool-use loops
	// keep working when thinking is enabled (Phase 3 F).
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Image content (Type=="image"). Source is the Anthropic-shaped
	// object: {type, media_type, data} for base64 inline. Follow-up item 1.
	Source *imageSource `json:"source,omitempty"`
}

// imageSource is the wire-level shape of an image-block source. Kept
// unexported so the exposed surface for callers is the typed
// ContentBlock / ImageSource pair in contentblock.go.
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// contentBlockFromConvBlock converts an exported ContentBlock (from a
// caller's ConversationTurn) into the wire-level contentBlock used in
// the request body. Follow-up item 1.
func contentBlockFromConvBlock(cb ContentBlock) contentBlock {
	switch cb.Type {
	case "image":
		// Correct a stale declared media type against the payload's actual
		// magic bytes (heals conversations persisted before capture-time
		// sniffing; the API 400s on a mismatch).
		cb = HealImageBlock(cb)
		out := contentBlock{Type: "image"}
		if cb.Source != nil {
			out.Source = &imageSource{
				Type:      cb.Source.Type,
				MediaType: cb.Source.MediaType,
				Data:      cb.Source.Data,
				URL:       cb.Source.URL,
				FileID:    cb.Source.FileID,
			}
		}
		return out
	default:
		// "text" or anything unrecognised is rendered as a text block.
		return contentBlock{Type: "text", Text: cb.Text}
	}
}

type agenticToolDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// ---------- response types ----------

type agenticResponseBody struct {
	ID         string                `json:"id"`
	Type       string                `json:"type"`
	Role       string                `json:"role"`
	Content    []agenticContentBlock `json:"content"`
	Model      string                `json:"model"`
	StopReason string                `json:"stop_reason"`
	Usage      *agenticUsage         `json:"usage,omitempty"`
	Error      *apiError             `json:"error,omitempty"`
}

// agenticUsage mirrors the Anthropic Messages API "usage" object.
type agenticUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type agenticContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`  // Phase 3 F: thinking block content
	Signature string          `json:"signature,omitempty"` // Phase 3 F: thinking block signature
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type apiErrorResponse struct {
	Type  string    `json:"type"`
	Error *apiError `json:"error"`
}

// CompleteWithTools sends a multi-turn conversation with tool definitions to the
// Anthropic Messages API and returns the structured response. Transient API
// errors (429/529/5xx, network) are retried per the provider's RetryPolicy;
// when the policy is exhausted and a model fallback is configured, ONE final
// attempt is made against the fallback model before returning the error.
func (c *ClaudeAgenticProvider) CompleteWithTools(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
) (*AgenticResponse, error) {
	return c.runWithRetryAndFallback(ctx, func(cc *ClaudeAgenticProvider, ctx context.Context, model string) (*AgenticResponse, retryClassification, time.Duration, error) {
		return cc.completeOnce(ctx, conversation, tools, systemPrompt, model)
	})
}

// claudeAttempt is one single-attempt runner against a named model. cc is the
// provider variant the attempt must use (it differs from the entry-point
// receiver after the effort self-heal strips effort from a copy).
type claudeAttempt func(cc *ClaudeAgenticProvider, ctx context.Context, model string) (*AgenticResponse, retryClassification, time.Duration, error)

// runWithRetryAndFallback wraps a single-attempt runner with the provider's
// resilience semantics, shared verbatim by all four entry points
// (CompleteWithTools, CompleteWithToolsStream, and the native Chat /
// ChatStream): the retry policy, the effort self-heal, and the one-shot model
// fallback.
func (c *ClaudeAgenticProvider) runWithRetryAndFallback(ctx context.Context, attempt claudeAttempt) (*AgenticResponse, error) {
	resp, err := retryWithPolicy(ctx, c.retryPolicy, func(ctx context.Context, _ int) (*AgenticResponse, retryClassification, time.Duration, error) {
		return attempt(c, ctx, c.model)
	})
	// Effort self-heal: some models (e.g. Haiku 4.5) reject output_config.effort
	// with a 400. Rather than bricking every leg of a run whose seat paired
	// effort with such a model, strip the effort and retry once.
	if err != nil && c.effort != "" && IsEffortUnsupportedErr(err) {
		bare := *c
		bare.effort = ""
		return bare.runWithRetryAndFallback(ctx, attempt)
	}
	if err == nil {
		return resp, nil
	}
	// One-shot model fallback if configured. Skipped for HTTP 400
	// invalid_request errors: the payload itself is rejected, so re-sending
	// it to a different model deterministically fails the same way (the
	// model-specific effort 400 is already self-healed above).
	if c.modelFallback.Secondary != "" && c.modelFallback.Secondary != c.model && !IsInvalidRequestErr(err) {
		fbResp, _, _, fbErr := attempt(c, ctx, c.modelFallback.Secondary)
		if fbErr == nil {
			return fbResp, nil
		}
	}
	return nil, err
}

// completeOnce performs a single attempt against the named model. Returns
// the parsed response plus a classification so retryWithPolicy can decide
// whether to retry. Used by both CompleteWithTools and the fallback path.
func (c *ClaudeAgenticProvider) completeOnce(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
	model string,
) (*AgenticResponse, retryClassification, time.Duration, error) {
	messages := c.buildMessages(conversation)
	toolDefs := c.buildToolDefs(tools)
	return c.doComplete(ctx, c.newAgenticRequest(messages, toolDefs, systemPrompt, model, false))
}

// newAgenticRequest assembles one Messages-API request body from prebuilt
// messages/tool definitions, applying the prompt-cache breakpoints. Shared by
// the shim path (completeOnce/streamOnce over ConversationTurn) and the
// native ChatProvider path (chatOnce/chatStreamOnce over neutral Turns) so
// both produce byte-identical bodies from equivalent content.
func (c *ClaudeAgenticProvider) newAgenticRequest(
	messages []agenticMessage,
	toolDefs []agenticToolDef,
	systemPrompt string,
	model string,
	stream bool,
) agenticRequest {
	var systemField interface{}
	if c.cacheEnabled {
		// Place cache breakpoints on the stable prefix (system + tools) and on
		// the trailing conversation block. The system + ~48 tool schemas are
		// re-sent on every loop iteration, so caching that prefix is the main
		// cost/latency win; the trailing breakpoint lets the growing
		// conversation be cached incrementally across turns.
		systemField = buildSystemBlocks(systemPrompt)
		applyToolCacheBreakpoint(toolDefs)
		applyTrailingMessageCacheBreakpoint(messages)
	} else if systemPrompt != "" {
		systemField = systemPrompt
	}

	return agenticRequest{
		Model:        model,
		MaxTokens:    c.maxTokens,
		System:       systemField,
		Messages:     messages,
		Tools:        toolDefs,
		Stream:       stream,
		Thinking:     c.thinking,
		OutputConfig: c.outputConfig(),
	}
}

// doComplete performs one non-streaming HTTP round trip for an assembled
// request body and parses the response. Shared transport for the shim and
// native paths.
func (c *ClaudeAgenticProvider) doComplete(ctx context.Context, reqBody agenticRequest) (*AgenticResponse, retryClassification, time.Duration, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, classFatal, 0, fmt.Errorf("claude-agentic: marshal request: %w", err)
	}

	endpoint := c.apiURL + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, classFatal, 0, fmt.Errorf("claude-agentic: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err), 0, fmt.Errorf("claude-agentic: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, readErr := io.ReadAll(resp.Body)
	class, retryAfter := classifyHTTPError(resp.StatusCode, resp.Header)
	if class != classSuccess {
		var errResp apiErrorResponse
		if json.Unmarshal(respBytes, &errResp) == nil && errResp.Error != nil {
			return nil, class, retryAfter, fmt.Errorf("claude-agentic: %s (HTTP %d): %s",
				errResp.Error.Type, resp.StatusCode, errResp.Error.Message)
		}
		return nil, class, retryAfter, fmt.Errorf("claude-agentic: HTTP %d: %s", resp.StatusCode, string(respBytes))
	}
	if readErr != nil {
		return nil, classTransient, 0, fmt.Errorf("claude-agentic: read response: %w", readErr)
	}

	var body agenticResponseBody
	if err := json.Unmarshal(respBytes, &body); err != nil {
		return nil, classFatal, 0, fmt.Errorf("claude-agentic: unmarshal response: %w", err)
	}

	return c.parseResponse(&body), classSuccess, 0, nil
}

// HealConversationHead returns turns with leading entries removed up to the
// first genuine user turn (Role == "user"). The Anthropic Messages API requires
// the first message to be a user message and rejects a tool_result block whose
// tool_use is not in the previous message ("unexpected tool_use_id found in
// tool_result blocks"). A conversation window cut between an assistant tool_use
// and its "tool" result — e.g. by a naive tail-trim — leaves such a dangling
// head, so it is trimmed here. Conversations that already begin with a user
// turn (and degenerate ones with no user turn at all) are returned unchanged.
func HealConversationHead(turns []ConversationTurn) []ConversationTurn {
	i := 0
	for i < len(turns) && turns[i].Role != "user" {
		i++
	}
	if i >= len(turns) {
		// No user turn at all (degenerate); leave as-is rather than emptying.
		return turns
	}
	return turns[i:]
}

// buildMessages converts ConversationTurn slices into the Claude messages format.
func (c *ClaudeAgenticProvider) buildMessages(conversation []ConversationTurn) []agenticMessage {
	var messages []agenticMessage

	// Defensive: never emit a leading orphaned tool_result (or an assistant-first
	// transcript). Callers heal their stored conversation too, but this is the
	// single chokepoint every request passes through.
	conversation = HealConversationHead(conversation)
	// Repair mid-transcript tool_use/tool_result pairing damage (interrupted
	// rounds, compaction/KV-window splices): synthesize missing results, drop
	// orphaned ones. Same chokepoint rationale — no caller can bypass it.
	conversation = HealToolPairing(conversation)

	for ti := 0; ti < len(conversation); ti++ {
		turn := conversation[ti]
		switch turn.Role {
		case "user":
			// Follow-up item 1: when ContentBlocks is non-empty (e.g. an
			// image attached via ##image), emit the structured Anthropic
			// content array. Otherwise stay on the string form so the
			// non-multimodal path is bit-identical to its pre-item-1 shape.
			if len(turn.ContentBlocks) > 0 {
				blocks := make([]contentBlock, 0, len(turn.ContentBlocks))
				for _, cb := range turn.ContentBlocks {
					blocks = append(blocks, contentBlockFromConvBlock(cb))
				}
				// If the turn also carries a Content string, prepend it as a
				// leading text block so a caller can mix typed text with
				// images cleanly.
				if turn.Content != "" {
					blocks = append([]contentBlock{{Type: "text", Text: turn.Content}}, blocks...)
				}
				messages = append(messages, agenticMessage{
					Role:    "user",
					Content: blocks,
				})
			} else {
				messages = append(messages, agenticMessage{
					Role:    "user",
					Content: turn.Content,
				})
			}

		case "assistant":
			var blocks []contentBlock
			// Replay extended-thinking blocks FIRST: when thinking is enabled
			// the Messages API requires the replayed assistant turn to start
			// with its thinking block(s) (signature intact) before any
			// text / tool_use blocks. Turns recorded without thinking (or with
			// thinking disabled) skip this loop entirely.
			for _, tb := range turn.Thinking {
				if tb.Text == "" && tb.Signature == "" {
					continue
				}
				blocks = append(blocks, contentBlock{
					Type:      "thinking",
					Thinking:  tb.Text,
					Signature: tb.Signature,
				})
			}
			if turn.Content != "" {
				blocks = append(blocks, contentBlock{
					Type: "text",
					Text: turn.Content,
				})
			}
			for _, tc := range turn.ToolCalls {
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: sanitizeToolInput(tc.Input),
				})
			}
			if len(blocks) > 0 {
				messages = append(messages, agenticMessage{
					Role:    "assistant",
					Content: blocks,
				})
			}

		case "tool":
			// Consume the ENTIRE run of consecutive tool turns and emit ONE
			// user message with all their tool_result blocks. The Messages
			// API requires every tool_use of the preceding assistant message
			// to be answered in the IMMEDIATELY following message — emitting
			// one message per result breaks any round with parallel tool
			// calls ("`tool_use` ids were found without `tool_result` blocks
			// immediately after", HTTP 400).
			var resultBlocks []contentBlock
			var attachments []ContentBlock
			for ti < len(conversation) && conversation[ti].Role == "tool" {
				tr := conversation[ti]
				resultBlocks = append(resultBlocks, contentBlock{
					Type:      "tool_result",
					ToolUseID: tr.ToolCallID,
					Content:   tr.Content,
					IsError:   tr.IsError,
				})
				// Structured attachments (e.g. the screenshot captured by
				// browser_action) are collected and delivered as ONE
				// follow-up user message after the results message, so the
				// model still SEES the captures without splitting the
				// tool_result batch. Each image block is healed first: a
				// conversation restored from session KV may carry a stale
				// declared media type (pre-sniffer captures), and the API
				// 400s when it doesn't match the magic bytes.
				for _, cb := range tr.ContentBlocks {
					attachments = append(attachments, HealImageBlock(cb))
				}
				ti++
			}
			ti-- // the outer loop's ti++ steps past the last consumed turn
			messages = append(messages, agenticMessage{
				Role:    "user",
				Content: resultBlocks,
			})
			if len(attachments) > 0 {
				messages = append(messages, agenticMessage{
					Role:    "user",
					Content: attachments,
				})
			}
		}
	}

	return messages
}

// buildSystemBlocks converts the system prompt into the structured array form
// with a cache_control breakpoint on the final block. Returns nil when the
// prompt is empty so the system field is omitted entirely.
func buildSystemBlocks(systemPrompt string) []systemBlock {
	if systemPrompt == "" {
		return nil
	}
	return []systemBlock{{
		Type:         "text",
		Text:         systemPrompt,
		CacheControl: ephemeralCache,
	}}
}

// applyToolCacheBreakpoint marks the last tool definition as a cache
// breakpoint, which caches the entire (stable) tools array prefix.
func applyToolCacheBreakpoint(defs []agenticToolDef) {
	if len(defs) == 0 {
		return
	}
	defs[len(defs)-1].CacheControl = ephemeralCache
}

// applyTrailingMessageCacheBreakpoint marks the final content block of the last
// message as a cache breakpoint so the conversation-so-far is cached and read
// back on the next turn. String-content messages are converted to a single
// text block so the breakpoint can be attached.
func applyTrailingMessageCacheBreakpoint(messages []agenticMessage) {
	// Breakpoints on the last TWO messages: the final message can be an
	// ephemeral one that changes every round (e.g. the current-screenshot
	// injection), so a breakpoint there alone would never be read back. The
	// second-to-last marks the end of the stable stored prefix — that is the
	// boundary the next round's request actually re-reads. Anthropic allows
	// up to 4 breakpoints; system + tools use two, these use the other two.
	for _, idx := range []int{len(messages) - 2, len(messages) - 1} {
		if idx < 0 {
			continue
		}
		markMessageCacheBreakpoint(&messages[idx])
	}
}

// markMessageCacheBreakpoint attaches cache_control to a message's final
// content block, converting string content to a text block when needed.
func markMessageCacheBreakpoint(m *agenticMessage) {
	switch content := m.Content.(type) {
	case []contentBlock:
		if len(content) > 0 {
			content[len(content)-1].CacheControl = ephemeralCache
		}
	case string:
		m.Content = []contentBlock{{
			Type:         "text",
			Text:         content,
			CacheControl: ephemeralCache,
		}}
	}
}

// emptyObjectSchema is the fallback JSON Schema for tools that declare no
// (or an unusable) parameter schema. input_schema is a required field on
// Anthropic tool definitions, so it must always be a valid object.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// sanitizeToolInput returns a marshal-safe tool_use input block. A
// conversation restored from session KV can carry an empty or truncated
// Input (e.g. a stream that died mid input_json_delta, persisted before the
// parser guarded against it). json.RawMessage.MarshalJSON fails on such
// bytes ("unexpected end of JSON input" / "invalid character ..."), which
// bricks EVERY subsequent request that replays the turn. Substituting the
// empty object heals the conversation at the single chokepoint all requests
// pass through.
func sanitizeToolInput(in json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(in)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return json.RawMessage(`{}`)
	}
	return in
}

// buildToolDefs converts ToolSpec slices into Claude tool definitions.
func (c *ClaudeAgenticProvider) buildToolDefs(tools []ToolSpec) []agenticToolDef {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]agenticToolDef, 0, len(tools))
	for _, t := range tools {
		// input_schema has no omitempty (it is required by the API), so an
		// empty or invalid Parameters RawMessage would fail the whole request
		// at marshal time. Fall back to the empty-object schema instead.
		schema := t.Parameters
		if trimmed := bytes.TrimSpace(schema); len(trimmed) == 0 || !json.Valid(trimmed) {
			schema = emptyObjectSchema
		}
		defs = append(defs, agenticToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return defs
}

// parseResponse converts the raw API response into a structured AgenticResponse.
func (c *ClaudeAgenticProvider) parseResponse(body *agenticResponseBody) *AgenticResponse {
	result := &AgenticResponse{
		StopReason: StopReason(body.StopReason),
	}

	if body.Usage != nil {
		result.Usage = Usage{
			InputTokens:              body.Usage.InputTokens,
			OutputTokens:             body.Usage.OutputTokens,
			CacheReadInputTokens:     body.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: body.Usage.CacheCreationInputTokens,
		}
	}

	for _, block := range body.Content {
		switch block.Type {
		case "text":
			result.TextBlocks = append(result.TextBlocks, TextBlock{Text: block.Text})
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, ToolCallRequest{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		case "thinking":
			// Phase 3 F: extended-thinking block. Preserves the signature so
			// the caller can echo it back on follow-up turns if the API
			// requires it for chained-thinking workflows.
			result.ThinkingBlocks = append(result.ThinkingBlocks, ThinkingBlock{
				Text:      block.Thinking,
				Signature: block.Signature,
			})
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// StaticProvider — fallback for testing or unconfigured deployments.
// ---------------------------------------------------------------------------

// StaticAgenticProvider returns a fixed response. Used when no API key is set.
type StaticAgenticProvider struct {
	Response string
}

func (s *StaticAgenticProvider) Name() string { return "static" }

func (s *StaticAgenticProvider) Complete(_ context.Context, _ string) (string, error) {
	return s.Response, nil
}

func (s *StaticAgenticProvider) CompleteWithTools(
	_ context.Context,
	_ []ConversationTurn,
	_ []ToolSpec,
	_ string,
) (*AgenticResponse, error) {
	return &AgenticResponse{
		TextBlocks: []TextBlock{{Text: s.Response}},
		StopReason: StopReasonEndTurn,
	}, nil
}
