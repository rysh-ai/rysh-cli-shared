// SPDX-License-Identifier: Apache-2.0

package provider

// Design 002 A1 (final step) — the native Anthropic ChatProvider
// implementation. Chat/ChatStream translate neutral Turn/Block history
// straight to the Messages-API wire format; ConversationTurn never appears in
// this path. The wire bytes are BYTE-IDENTICAL to the compat-adapter path
// (AsChatProvider over a wrapper -> ConversationFromTurns -> buildMessages)
// for every expressible ChatRequest: the healing chokepoints
// (HealConversationHead / HealToolPairing / HealImageBlock) and the emission
// rules below are faithful ports of the shim builders, applied at the same
// point of the pipeline, and the differential tests in chat_native_test.go
// pin the equivalence over fixed and randomized corpora.

import (
	"context"
	"fmt"
	"time"
)

// nativeChatSelf marks the provider's ChatProvider implementation as native
// (its own wire translation, no shim conversion). AsChatProvider identity-
// checks the returned receiver so wrappers that EMBED this provider — and
// would otherwise satisfy ChatProvider through method promotion, silently
// bypassing their own decoration — never take the fast path.
func (c *ClaudeAgenticProvider) nativeChatSelf() ChatProvider { return c }

// Chat implements ChatProvider natively: neutral turns -> Anthropic Messages
// request -> neutral response. Per-request Model/MaxTokens overrides route
// through the same seams as the adapter path (0 keeps the construction-time
// cap; invalid values error loudly), and the retry policy, effort self-heal,
// and model fallback apply exactly as on CompleteWithTools.
func (c *ClaudeAgenticProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.runWithRetryAndFallback(ctx, func(cc *ClaudeAgenticProvider, ctx context.Context, model string) (*AgenticResponse, retryClassification, time.Duration, error) {
		return cc.chatOnce(ctx, req, model)
	})
	return ChatResponseFromAgentic(ar), err
}

// ChatStream implements ChatProvider natively; the SSE consumption and
// StreamEvent sequence are the same code path CompleteWithToolsStream uses.
func (c *ClaudeAgenticProvider) ChatStream(ctx context.Context, req ChatRequest, cb StreamCallback) (*ChatResponse, error) {
	cc, err := c.chatRequestTarget(req)
	if err != nil {
		return nil, err
	}
	ar, err := cc.runWithRetryAndFallback(ctx, func(cc *ClaudeAgenticProvider, ctx context.Context, model string) (*AgenticResponse, retryClassification, time.Duration, error) {
		return cc.chatStreamOnce(ctx, req, model, cb)
	})
	return ChatResponseFromAgentic(ar), err
}

// ChatStreamSupported reports genuine SSE streaming support.
func (c *ClaudeAgenticProvider) ChatStreamSupported() bool { return true }

// chatRequestTarget resolves the provider variant a ChatRequest should hit,
// applying the per-request Model / MaxTokens overrides through the exact same
// seam (and with the exact same error semantics) as the compat adapter's
// chatTarget.
func (c *ClaudeAgenticProvider) chatRequestTarget(req ChatRequest) (*ClaudeAgenticProvider, error) {
	target, err := chatTarget(c, req)
	if err != nil {
		return nil, err
	}
	cc, ok := target.(*ClaudeAgenticProvider)
	if !ok {
		// Unreachable for the bare provider (its override seams return copies
		// of itself); guarded so a future seam change fails loudly.
		return nil, fmt.Errorf("claude-agentic: per-request override produced unexpected provider type %T", target)
	}
	return cc, nil
}

// chatOnce runs one non-streaming native attempt against the named model.
func (c *ClaudeAgenticProvider) chatOnce(ctx context.Context, req ChatRequest, model string) (*AgenticResponse, retryClassification, time.Duration, error) {
	messages := c.buildTurnMessages(req.Turns)
	toolDefs := c.buildToolDefs(req.Tools)
	return c.doComplete(ctx, c.newAgenticRequest(messages, toolDefs, req.System, model, false))
}

// chatStreamOnce runs one streaming native attempt against the named model.
func (c *ClaudeAgenticProvider) chatStreamOnce(ctx context.Context, req ChatRequest, model string, cb StreamCallback) (*AgenticResponse, retryClassification, time.Duration, error) {
	messages := c.buildTurnMessages(req.Turns)
	toolDefs := c.buildToolDefs(req.Tools)
	return c.doStream(ctx, c.newAgenticRequest(messages, toolDefs, req.System, model, true), cb)
}

// buildTurnMessages converts neutral turns into Claude wire messages: flatten
// to message-granular units, heal (head + tool pairing, the same chokepoints
// buildMessages applies to every shim request), then emit. Equivalent to
// buildMessages(ConversationFromTurns(turns)) byte-for-byte.
func (c *ClaudeAgenticProvider) buildTurnMessages(turns []Turn) []agenticMessage {
	units := flattenTurns(turns)
	units = healUnitsHead(units)
	units = healUnitsToolPairing(units)
	return c.messagesFromUnits(units)
}

// healUnitsHead removes leading units up to the first genuine user unit —
// the native port of HealConversationHead (see there for the API rationale).
// Sequences with no user unit at all are returned unchanged.
func healUnitsHead(units []wireTurn) []wireTurn {
	i := 0
	for i < len(units) && units[i].role != "user" {
		i++
	}
	if i >= len(units) {
		return units
	}
	return units[i:]
}

// healUnitsToolPairing repairs tool_use/tool_result pairing damage — the
// native port of HealToolPairing (see toolpairing.go for the invariants):
// missing results are synthesized in ToolCalls order after any real results,
// orphaned results are dropped, duplicates keep the first.
func healUnitsToolPairing(units []wireTurn) []wireTurn {
	out := make([]wireTurn, 0, len(units))
	i := 0
	for i < len(units) {
		u := units[i]
		switch {
		case u.role == "assistant" && len(u.toolCalls) > 0:
			out = append(out, u)
			wanted := make(map[string]bool, len(u.toolCalls))
			for _, tc := range u.toolCalls {
				wanted[tc.ID] = true
			}
			answered := make(map[string]bool, len(u.toolCalls))
			j := i + 1
			for j < len(units) && units[j].role == "tool" {
				tr := units[j]
				if wanted[tr.result.CallID] && !answered[tr.result.CallID] {
					answered[tr.result.CallID] = true
					out = append(out, tr)
				}
				// else: orphan for a different call, or a duplicate — drop.
				j++
			}
			for _, tc := range u.toolCalls {
				if !answered[tc.ID] {
					out = append(out, wireTurn{
						role: "tool",
						result: ToolResult{
							CallID:  tc.ID,
							Content: healedToolResultNote,
							IsError: true,
						},
					})
				}
			}
			i = j
		case u.role == "tool":
			// A tool unit with no owning assistant tool_use immediately
			// before it — the API would reject it. Drop.
			i++
		default:
			out = append(out, u)
			i++
		}
	}
	return out
}

// messagesFromUnits emits Claude wire messages from healed units — the
// native port of buildMessages' emission loop (same block order, same
// string-vs-array content forms, same tool-result batching and attachment
// follow-up message, same image healing).
func (c *ClaudeAgenticProvider) messagesFromUnits(units []wireTurn) []agenticMessage {
	var messages []agenticMessage

	for ti := 0; ti < len(units); ti++ {
		u := units[ti]
		switch u.role {
		case "user":
			// Attachments present: emit the structured content array (with
			// the promoted text, when any, as the leading text block).
			// Otherwise stay on the string form so the non-multimodal path is
			// bit-identical to the shim builder's.
			if len(u.blocks) > 0 {
				blocks := make([]contentBlock, 0, len(u.blocks))
				for _, cb := range u.blocks {
					blocks = append(blocks, contentBlockFromConvBlock(cb))
				}
				if u.content != "" {
					blocks = append([]contentBlock{{Type: "text", Text: u.content}}, blocks...)
				}
				messages = append(messages, agenticMessage{
					Role:    "user",
					Content: blocks,
				})
			} else {
				messages = append(messages, agenticMessage{
					Role:    "user",
					Content: u.content,
				})
			}

		case "assistant":
			var blocks []contentBlock
			// Thinking blocks FIRST (signature intact) — the Messages API
			// requires replayed assistant turns to lead with them when
			// extended thinking is enabled.
			for _, tb := range u.thinking {
				if tb.Text == "" && tb.Signature == "" {
					continue
				}
				blocks = append(blocks, contentBlock{
					Type:      "thinking",
					Thinking:  tb.Text,
					Signature: tb.Signature,
				})
			}
			if u.content != "" {
				blocks = append(blocks, contentBlock{
					Type: "text",
					Text: u.content,
				})
			}
			for _, tc := range u.toolCalls {
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: sanitizeToolInput(tc.ParamsJSON),
				})
			}
			if len(blocks) > 0 {
				messages = append(messages, agenticMessage{
					Role:    "assistant",
					Content: blocks,
				})
			}

		case "tool":
			// Consume the ENTIRE run of consecutive tool units and emit ONE
			// user message with all their tool_result blocks (parallel tool
			// calls must be answered in a single message), plus ONE follow-up
			// user message carrying the run's attachments, if any.
			var resultBlocks []contentBlock
			var attachments []ContentBlock
			for ti < len(units) && units[ti].role == "tool" {
				tr := units[ti]
				resultBlocks = append(resultBlocks, contentBlock{
					Type:      "tool_result",
					ToolUseID: tr.result.CallID,
					Content:   tr.result.Content,
					IsError:   tr.result.IsError,
				})
				for _, cb := range tr.blocks {
					attachments = append(attachments, HealImageBlock(cb))
				}
				ti++
			}
			ti-- // the outer loop's ti++ steps past the last consumed unit
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
