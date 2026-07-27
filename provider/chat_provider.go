package provider

// Design 002 A2 — the widened, provider-neutral chat interface.
//
// ChatProvider is what the orchestrator's provider boundary compiles against:
// neutral history (Turn/Block, turns.go), tool definitions, a system prompt,
// and a neutral response (blocks + usage + stop reason). AgenticProvider and
// ConversationTurn remain untouched as the compatibility shim; AsChatProvider
// adapts ANY AgenticProvider (Claude, OpenAI-compatible, secretnat-wrapped,
// rysh-cli decorators, mocks) into a ChatProvider at the edge using the
// turns.go converters, so nothing implementing the old interface needs to
// change. The two concrete providers in this package implement ChatProvider
// NATIVELY (claude_chat_native.go / openai_chat_native.go): they translate
// neutral turns straight to their own wire dialects, byte-identical to what
// the adapter path produces (pinned by chat_native_test.go), and
// AsChatProvider fast-paths to them when handed the bare provider.
//
// Streaming reuses the existing provider-neutral StreamEvent/StreamCallback
// contract from streaming.go — it is already shared verbatim by the Claude
// and OpenAI-compatible streamers, so ChatStream emits exactly the same event
// sequence the shim path does.

import (
	"context"
	"fmt"
)

// ChatRequest is one neutral chat completion request.
type ChatRequest struct {
	// System is the system prompt ("" for none).
	System string
	// Turns is the neutral conversation history, oldest first.
	Turns []Turn
	// Tools are the tool definitions offered to the model (nil for none).
	Tools []ToolSpec
	// Model optionally overrides the provider's configured model for this
	// request. Routed through ModelEffortOverridable; providers without that
	// seam return an error rather than silently ignoring the override.
	Model string
	// MaxTokens optionally caps the response size (max_tokens on the wire)
	// for this request. Routed through MaxTokensOverridable; 0 keeps the
	// provider's construction-time cap, and providers without that seam
	// return an error rather than silently ignoring the cap.
	MaxTokens int
}

// ChatResponse is one neutral chat completion response. Blocks preserve the
// model's output order where the source can express it; responses adapted
// from an AgenticResponse carry the canonical order thinking, text,
// tool calls (AgenticResponse does not record cross-kind ordering).
type ChatResponse struct {
	Blocks     []Block
	StopReason StopReason
	Usage      Usage
}

// ChatProvider is the neutral multi-turn tool-use provider interface
// (design 002 §3.2). Chat is the one-shot path; ChatStream is the
// incremental path and MUST behave identically apart from delivering
// StreamEvents to cb along the way (implementations without true streaming
// may satisfy it by completing one-shot and delivering no events — callers
// discover real streaming support via ChatStreamSupported).
type ChatProvider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, cb StreamCallback) (*ChatResponse, error)
}

// chatStreamCapable is the optional self-report a ChatProvider implements to
// declare whether ChatStream delivers genuine incremental events.
type chatStreamCapable interface {
	ChatStreamSupported() bool
}

// ChatStreamSupported reports whether cp streams incrementally. Providers
// that don't self-report are assumed capable (they implement ChatStream by
// contract); the compat adapter self-reports by type-asserting the wrapped
// provider against StreamingProvider, so the answer matches the shim path's
// capability check exactly.
func ChatStreamSupported(cp ChatProvider) bool {
	if c, ok := cp.(chatStreamCapable); ok {
		return c.ChatStreamSupported()
	}
	return cp != nil
}

// ---------------------------------------------------------------------------
// Compat adapter: AgenticProvider -> ChatProvider
// ---------------------------------------------------------------------------

// nativeChatCapable is the opt-in marker for providers whose Chat/ChatStream
// translate neutral turns to their own wire format natively (no shim
// conversion). nativeChatSelf returns the implementing receiver;
// AsChatProvider identity-checks it against the passed provider, so the
// marker can only ever fast-path the provider that actually implements it.
// The method is unexported on purpose: outside this package it can only be
// obtained by embedding a native provider, and the identity check defeats
// exactly that case.
type nativeChatCapable interface {
	nativeChatSelf() ChatProvider
}

// AsChatProvider adapts p into a ChatProvider. The two bare native providers
// (Claude / OpenAI-compatible) are returned as-is — their Chat/ChatStream
// translate neutral turns straight to the wire, no converters involved.
// Everything else — wrappers included — is adapted at the edge via the
// neutral converters, so decorator semantics (sanitization, defaults,
// overrides) keep applying. A wrapper that EMBEDS a native provider inherits
// nativeChatSelf by promotion, but the promoted method returns the embedded
// provider, not the wrapper, so the identity check below keeps it on the
// adapter path instead of silently bypassing its decoration. Nil in, nil out.
func AsChatProvider(p AgenticProvider) ChatProvider {
	if p == nil {
		return nil
	}
	if n, ok := p.(nativeChatCapable); ok {
		if self := n.nativeChatSelf(); self != nil && any(self) == any(p) {
			return self
		}
	}
	return &agenticChatAdapter{inner: p}
}

// agenticChatAdapter converts neutral requests to the shim shape at the
// provider edge. It adds no behavior: the wrapped provider builds the exact
// same wire request it would have built from the equivalent shim call.
type agenticChatAdapter struct {
	inner AgenticProvider
}

func (a *agenticChatAdapter) Name() string { return a.inner.Name() }

func (a *agenticChatAdapter) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return chatViaAgentic(ctx, a.inner, req)
}

func (a *agenticChatAdapter) ChatStream(ctx context.Context, req ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	return chatStreamViaAgentic(ctx, a.inner, req, cb)
}

// ChatStreamSupported mirrors the shim path's capability check: streaming is
// available exactly when the wrapped provider implements StreamingProvider.
func (a *agenticChatAdapter) ChatStreamSupported() bool {
	_, ok := a.inner.(StreamingProvider)
	return ok
}

// chatTarget resolves the provider a request should hit, applying the
// per-request model and max-tokens overrides when present. Unsupported
// overrides error loudly — never a silent drop.
func chatTarget(p AgenticProvider, req ChatRequest) (AgenticProvider, error) {
	if req.MaxTokens < 0 {
		return nil, fmt.Errorf("chat: provider %q: negative MaxTokens %d is invalid", p.Name(), req.MaxTokens)
	}
	if req.Model != "" {
		mo, ok := p.(ModelEffortOverridable)
		if !ok {
			return nil, fmt.Errorf("chat: provider %q does not support a per-request model override", p.Name())
		}
		p = mo.WithModelEffort(req.Model, "")
	}
	if req.MaxTokens > 0 {
		mt, ok := p.(MaxTokensOverridable)
		if !ok {
			return nil, fmt.Errorf("chat: provider %q does not support a per-request MaxTokens override; configure max tokens at provider construction", p.Name())
		}
		capped := mt.WithMaxTokens(req.MaxTokens)
		if capped == nil {
			// A wrapper forwards the seam but its wrapped provider lacks it.
			return nil, fmt.Errorf("chat: provider %q cannot honor the per-request MaxTokens override (wrapped provider lacks the seam)", p.Name())
		}
		p = capped
	}
	return p, nil
}

// chatViaAgentic is the shared one-shot path: neutral request -> shim call ->
// neutral response. A partial response accompanying an error is converted and
// returned alongside it, matching the shim contract.
func chatViaAgentic(ctx context.Context, p AgenticProvider, req ChatRequest) (*ChatResponse, error) {
	target, err := chatTarget(p, req)
	if err != nil {
		return nil, err
	}
	ar, err := target.CompleteWithTools(ctx, ConversationFromTurns(req.Turns), req.Tools, req.System)
	return ChatResponseFromAgentic(ar), err
}

// chatStreamViaAgentic is the shared streaming path. Providers without
// StreamingProvider fall back to the one-shot path with no events — the same
// degradation the orchestrator's shim path applies.
func chatStreamViaAgentic(ctx context.Context, p AgenticProvider, req ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	target, err := chatTarget(p, req)
	if err != nil {
		return nil, err
	}
	sp, ok := target.(StreamingProvider)
	if !ok {
		ar, err := target.CompleteWithTools(ctx, ConversationFromTurns(req.Turns), req.Tools, req.System)
		return ChatResponseFromAgentic(ar), err
	}
	ar, err := sp.CompleteWithToolsStream(ctx, ConversationFromTurns(req.Turns), req.Tools, req.System, cb)
	return ChatResponseFromAgentic(ar), err
}

// ---------------------------------------------------------------------------
// Response converters
// ---------------------------------------------------------------------------

// ChatResponseFromAgentic converts a shim response into neutral blocks in
// canonical replay order: thinking, text, tool calls. Nil-safe.
func ChatResponseFromAgentic(ar *AgenticResponse) *ChatResponse {
	if ar == nil {
		return nil
	}
	cr := &ChatResponse{StopReason: ar.StopReason, Usage: ar.Usage}
	for i := range ar.ThinkingBlocks {
		tb := ar.ThinkingBlocks[i]
		cr.Blocks = append(cr.Blocks, Block{Kind: BlockKindThinking, Thinking: &tb})
	}
	for _, tb := range ar.TextBlocks {
		cr.Blocks = append(cr.Blocks, Block{Kind: BlockKindText, Text: tb.Text})
	}
	for _, tc := range ar.ToolCalls {
		cr.Blocks = append(cr.Blocks, Block{Kind: BlockKindToolCall, ToolCall: &ToolCall{
			ID:         tc.ID,
			Name:       tc.Name,
			ParamsJSON: tc.Input,
		}})
	}
	return cr
}

// AgenticResponseFromChat converts a neutral response back into the shim
// shape, grouping blocks by kind (AgenticResponse cannot express cross-kind
// ordering — see ChatResponse). tool_result and image blocks cannot appear in
// a model RESPONSE under the shim contract and are converted to text blocks
// carrying their textual content (documented, tested — never silently
// dropped). Nil-safe.
func AgenticResponseFromChat(cr *ChatResponse) *AgenticResponse {
	if cr == nil {
		return nil
	}
	ar := &AgenticResponse{StopReason: cr.StopReason, Usage: cr.Usage}
	for _, b := range cr.Blocks {
		switch b.Kind {
		case BlockKindThinking:
			if b.Thinking != nil {
				ar.ThinkingBlocks = append(ar.ThinkingBlocks, *b.Thinking)
			}
		case BlockKindToolCall:
			if b.ToolCall != nil {
				ar.ToolCalls = append(ar.ToolCalls, ToolCallRequest{
					ID:    b.ToolCall.ID,
					Name:  b.ToolCall.Name,
					Input: b.ToolCall.ParamsJSON,
				})
			}
		case BlockKindToolResult:
			if b.ToolResult != nil {
				ar.TextBlocks = append(ar.TextBlocks, TextBlock{Text: b.ToolResult.Content})
			}
		default: // BlockKindText, BlockKindImage (text payload only), future kinds
			ar.TextBlocks = append(ar.TextBlocks, TextBlock{Text: b.Text})
		}
	}
	return ar
}

// ---------------------------------------------------------------------------
// Native implementations on the concrete providers
// ---------------------------------------------------------------------------

// The concrete providers implement ChatProvider NATIVELY — neutral turns
// translated straight to their own wire dialects, no ConversationTurn hop —
// in claude_chat_native.go and openai_chat_native.go (design 002 A1 final
// step). Interface guards: ChatProvider plus the marker AsChatProvider
// fast-paths on, plus the per-request override seams ChatRequest routes
// through.
var (
	_ ChatProvider         = (*ClaudeAgenticProvider)(nil)
	_ ChatProvider         = (*OpenAIAgenticProvider)(nil)
	_ nativeChatCapable    = (*ClaudeAgenticProvider)(nil)
	_ nativeChatCapable    = (*OpenAIAgenticProvider)(nil)
	_ MaxTokensOverridable = (*ClaudeAgenticProvider)(nil)
	_ MaxTokensOverridable = (*OpenAIAgenticProvider)(nil)
)
