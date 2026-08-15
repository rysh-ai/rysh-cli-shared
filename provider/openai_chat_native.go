// SPDX-License-Identifier: Apache-2.0

package provider

// Design 002 A1 (final step) — the native OpenAI-compatible ChatProvider
// implementation (openai / ollama / gemini). Chat/ChatStream translate neutral
// Turn/Block history straight to the wire; ConversationTurn never appears in
// this path. The wire bytes are BYTE-IDENTICAL to the compat-adapter path
// (ConversationFromTurns -> the shim builders) for every expressible
// ChatRequest: flattenTurns reproduces the same message fan-out, and the unit
// emitters mirror their shim counterparts' per-message mapping (including the
// lossy drops — thinking blocks, attachments — neither dialect has ever
// carried). Pinned by the differential tests in chat_native_test.go.
//
// Two dialects live behind this one type, and the ROUTING must match
// CompleteWithTools exactly or the two paths diverge: OpenAI proper speaks the
// Responses API (openai_responses.go, unitToResponses), Ollama and the Gemini
// compat layer speak Chat Completions (buildTurnRequest, unitToOAI).

import (
	"context"
	"fmt"
)

// nativeChatSelf marks the ChatProvider implementation as native; see the
// Claude counterpart for the embedding-bypass rationale.
func (c *OpenAIAgenticProvider) nativeChatSelf() ChatProvider { return c }

// Chat implements ChatProvider natively: neutral turns -> wire request ->
// neutral response, in whichever dialect this endpoint speaks (OpenAI proper
// takes Responses, every other OpenAI-compatible endpoint takes Chat
// Completions — the same split CompleteWithTools applies, so the two paths do
// not diverge). Per-request Model/MaxTokens overrides route through the same
// seams (and error semantics) as the adapter path.
func (c *OpenAIAgenticProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.withRetry(ctx, func(ctx context.Context) (*AgenticResponse, error) {
		if cc.usesResponsesAPI() {
			return cc.doResponses(ctx, cc.buildTurnResponsesRequest(req.Turns, req.Tools, req.System))
		}
		return cc.doComplete(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System))
	})
	return ChatResponseFromAgentic(ar), err
}

// ChatStream implements ChatProvider natively; SSE consumption, the
// StreamEvent sequence, and the HTTP 400 non-streaming degradation are the
// same code paths CompleteWithToolsStream uses, per dialect.
func (c *OpenAIAgenticProvider) ChatStream(ctx context.Context, req ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.withStreamRetry(ctx, cb, func(ctx context.Context, cb StreamCallback) (*AgenticResponse, error) {
		if cc.usesResponsesAPI() {
			return cc.doResponsesStream(ctx, cc.buildTurnResponsesRequest(req.Turns, req.Tools, req.System), cb, func() (*AgenticResponse, error) {
				return cc.doResponses(ctx, cc.buildTurnResponsesRequest(req.Turns, req.Tools, req.System))
			})
		}
		return cc.doStream(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System), cb, func() (*AgenticResponse, error) {
			return cc.doComplete(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System))
		})
	})
	return ChatResponseFromAgentic(ar), err
}

// ChatStreamSupported reports genuine SSE streaming support.
func (c *OpenAIAgenticProvider) ChatStreamSupported() bool { return true }

// chatRequestTarget resolves the provider variant a ChatRequest should hit —
// see the Claude counterpart.
func (c *OpenAIAgenticProvider) chatRequestTarget(req ChatRequest) (*OpenAIAgenticProvider, error) {
	target, err := chatTarget(c, req)
	if err != nil {
		return nil, err
	}
	cc, ok := target.(*OpenAIAgenticProvider)
	if !ok {
		return nil, fmt.Errorf("%s: per-request override produced unexpected provider type %T", c.name, target)
	}
	return cc, nil
}

// buildTurnRequest translates neutral turns/tools into one Chat Completions
// request body. Equivalent to buildRequest(ConversationFromTurns(turns))
// byte-for-byte.
func (c *OpenAIAgenticProvider) buildTurnRequest(turns []Turn, tools []ToolSpec, systemPrompt string) oaiRequest {
	units := flattenTurns(turns)
	msgs := make([]oaiMessage, 0, len(units)+1)
	if systemPrompt != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: systemPrompt})
	}
	for _, u := range units {
		msgs = append(msgs, unitToOAI(u))
	}

	var oaiTools []oaiTool
	for _, ts := range tools {
		oaiTools = append(oaiTools, oaiTool{Type: "function", Function: oaiFunction{
			Name: ts.Name, Description: ts.Description, Params: ts.Parameters,
		}})
	}

	r := oaiRequest{Model: c.model, Messages: msgs, Tools: oaiTools}
	c.setMaxTokens(&r)
	return r
}

// unitToOAI converts one wire unit to an OpenAI message — the native mirror
// of turnToOAI (same role mapping, same drops: this dialect carries no
// thinking blocks or image/text attachments).
func unitToOAI(u wireTurn) oaiMessage {
	switch u.role {
	case "tool":
		return oaiMessage{Role: "tool", ToolCallID: u.result.CallID, Content: u.result.Content}
	case "assistant":
		m := oaiMessage{Role: "assistant", Content: u.content}
		for _, tc := range u.toolCalls {
			m.ToolCalls = append(m.ToolCalls, oaiToolCall{
				ID: tc.ID, Type: "function",
				Function: oaiFunction{Name: tc.Name, Arguments: string(tc.ParamsJSON)},
			})
		}
		return m
	default:
		return oaiMessage{Role: "user", Content: u.content}
	}
}
