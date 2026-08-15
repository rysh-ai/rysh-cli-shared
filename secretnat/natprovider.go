// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"context"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// NATProvider decorates an AgenticProvider with outbound sanitization: every
// request's conversation, tool inputs, and system prompt are deep-sanitized
// (on copies — the caller's conversation is never mutated) before reaching
// the wire. It performs NO inbound restore: synthetic tokens deliberately
// stay in the assistant turns so real values never enter the persisted
// conversation. The orchestrator restores just-in-time at tool execution and
// (optionally) for display.
//
// Sanitize is idempotent, so text already sanitized at source (tool results,
// user turns) passes through byte-identical — safe for prompt caching.
type NATProvider struct {
	inner provider.AgenticProvider
	nat   SessionHandle
}

// natStreamingProvider is the streaming-capable variant. Wrap returns it
// ONLY when the inner provider implements StreamingProvider, so the
// orchestrator's capability type-assert stays honest.
type natStreamingProvider struct {
	NATProvider
}

// unwrapper lets Wrap re-wrap an already-wrapped provider with a fresh
// session instead of stacking decorators.
type unwrapper interface {
	Unwrap() provider.AgenticProvider
}

// Wrap decorates p with s. Identity when p or s is nil. Re-wraps (never
// stacks) when p is already a NATProvider.
func Wrap(p provider.AgenticProvider, s SessionHandle) provider.AgenticProvider {
	if p == nil || s == nil {
		return p
	}
	if u, ok := p.(unwrapper); ok {
		p = u.Unwrap()
	}
	base := NATProvider{inner: p, nat: s}
	if _, ok := p.(provider.StreamingProvider); ok {
		return &natStreamingProvider{NATProvider: base}
	}
	return &base
}

// Unwrap returns the decorated provider.
func (n *NATProvider) Unwrap() provider.AgenticProvider { return n.inner }

// Name reports the inner provider's name (the decorator is transparent).
func (n *NATProvider) Name() string { return n.inner.Name() }

// Complete sanitizes the single-turn prompt outbound. The response is
// returned as-is (tokens intact).
func (n *NATProvider) Complete(ctx context.Context, prompt string) (string, error) {
	return n.inner.Complete(ctx, n.nat.Sanitize(prompt))
}

// CompleteWithTools sanitizes a copy of the conversation and the system
// prompt, then delegates.
func (n *NATProvider) CompleteWithTools(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
) (*provider.AgenticResponse, error) {
	conv, sys := n.sanitizeRequest(conversation, systemPrompt)
	return n.inner.CompleteWithTools(ctx, conv, tools, sys)
}

// CompleteWithToolsStream sanitizes outbound and delegates to the inner
// streaming provider. The callback passes through untouched — display
// restore (when enabled) is the orchestrator's concern.
func (n *natStreamingProvider) CompleteWithToolsStream(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
	cb provider.StreamCallback,
) (*provider.AgenticResponse, error) {
	conv, sys := n.sanitizeRequest(conversation, systemPrompt)
	return n.inner.(provider.StreamingProvider).CompleteWithToolsStream(ctx, conv, tools, sys, cb)
}

// WithModelEffort delegates the override to the inner provider and re-wraps
// the result, so per-run model/effort seats can never unwrap the NAT layer.
// When the inner provider doesn't support overrides, the receiver is
// returned unchanged (mirroring the orchestrator's fallback semantics).
func (n *NATProvider) WithModelEffort(model, effort string) provider.AgenticProvider {
	mo, ok := n.inner.(provider.ModelEffortOverridable)
	if !ok {
		return wrapSelf(n)
	}
	return Wrap(mo.WithModelEffort(model, effort), n.nat)
}

func (n *natStreamingProvider) WithModelEffort(model, effort string) provider.AgenticProvider {
	return n.NATProvider.WithModelEffort(model, effort)
}

// WithMaxTokens forwards the per-request max-tokens cap to the inner
// provider and re-wraps the result, so a capped seat can never unwrap the
// NAT layer. Unlike WithModelEffort there is NO silent fallback: when the
// inner provider lacks the seam, nil is returned per the
// MaxTokensOverridable contract so the caller fails loudly instead of
// sending a request with the cap silently dropped.
func (n *NATProvider) WithMaxTokens(maxTokens int) provider.AgenticProvider {
	mt, ok := n.inner.(provider.MaxTokensOverridable)
	if !ok {
		return nil
	}
	inner := mt.WithMaxTokens(maxTokens)
	if inner == nil {
		return nil
	}
	return Wrap(inner, n.nat)
}

func (n *natStreamingProvider) WithMaxTokens(maxTokens int) provider.AgenticProvider {
	return n.NATProvider.WithMaxTokens(maxTokens)
}

// wrapSelf re-establishes the streaming variant when needed (a *NATProvider
// method value can be reached from the embedded field of the streaming
// variant).
func wrapSelf(n *NATProvider) provider.AgenticProvider {
	if _, ok := n.inner.(provider.StreamingProvider); ok {
		return &natStreamingProvider{NATProvider: *n}
	}
	return n
}

// sanitizeRequest deep-copies and sanitizes the outbound request pieces.
// Thinking blocks are never rewritten (Anthropic verifies their signatures
// on replay; they were generated from already-sanitized input). Image blocks
// are skipped.
func (n *NATProvider) sanitizeRequest(
	conversation []provider.ConversationTurn,
	systemPrompt string,
) ([]provider.ConversationTurn, string) {
	if !n.nat.Enabled() {
		return conversation, systemPrompt
	}
	out := make([]provider.ConversationTurn, len(conversation))
	copy(out, conversation)
	for i := range out {
		t := &out[i]
		if t.Content != "" {
			t.Content = n.nat.Sanitize(t.Content)
		}
		if len(t.ContentBlocks) > 0 {
			blocks := make([]provider.ContentBlock, len(t.ContentBlocks))
			copy(blocks, t.ContentBlocks)
			for j := range blocks {
				if blocks[j].Type == "text" && blocks[j].Text != "" {
					blocks[j].Text = n.nat.Sanitize(blocks[j].Text)
				}
			}
			t.ContentBlocks = blocks
		}
		if len(t.ToolCalls) > 0 {
			calls := make([]provider.ToolCallRequest, len(t.ToolCalls))
			copy(calls, t.ToolCalls)
			for j := range calls {
				calls[j].Input = n.nat.SanitizeJSON(calls[j].Input)
			}
			t.ToolCalls = calls
		}
	}
	return out, n.nat.Sanitize(systemPrompt)
}
