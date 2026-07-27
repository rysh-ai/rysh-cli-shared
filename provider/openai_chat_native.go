package provider

// Design 002 A1 (final step) — the native OpenAI-compatible ChatProvider
// implementation (openai / ollama / gemini dialect). Chat/ChatStream
// translate neutral Turn/Block history straight to Chat Completions
// messages; ConversationTurn never appears in this path. The wire bytes are
// BYTE-IDENTICAL to the compat-adapter path (ConversationFromTurns ->
// buildRequest) for every expressible ChatRequest: flattenTurns reproduces
// the same message fan-out, and unitToOAI mirrors turnToOAI's per-message
// mapping (including its lossy drops — thinking blocks, attachments — which
// this dialect has never carried). Pinned by the differential tests in
// chat_native_test.go.

import (
	"context"
	"fmt"
)

// nativeChatSelf marks the ChatProvider implementation as native; see the
// Claude counterpart for the embedding-bypass rationale.
func (c *OpenAIAgenticProvider) nativeChatSelf() ChatProvider { return c }

// Chat implements ChatProvider natively: neutral turns -> Chat Completions
// request -> neutral response. Per-request Model/MaxTokens overrides route
// through the same seams (and error semantics) as the adapter path.
func (c *OpenAIAgenticProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.doComplete(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System))
	return ChatResponseFromAgentic(ar), err
}

// ChatStream implements ChatProvider natively; SSE consumption, the
// StreamEvent sequence, and the HTTP 400 non-streaming degradation are the
// same code path CompleteWithToolsStream uses.
func (c *OpenAIAgenticProvider) ChatStream(ctx context.Context, req ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.doStream(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System), cb, func() (*AgenticResponse, error) {
		return cc.doComplete(ctx, cc.buildTurnRequest(req.Turns, req.Tools, req.System))
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

	return oaiRequest{Model: c.model, Messages: msgs, Tools: oaiTools, MaxTokens: c.maxTokens}
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
