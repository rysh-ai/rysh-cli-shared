// SPDX-License-Identifier: Apache-2.0

package provider

// Design 002 A1 (final step) — native ChatProvider request building.
//
// The two concrete providers translate neutral Turn/Block history straight to
// their own wire dialects at the edge; ConversationTurn no longer appears
// anywhere in the ChatProvider path. What both dialects share is the GROUPING
// step: a neutral Turn is an ordered list of typed blocks, while both wire
// formats are message-granular (one content string / block list / tool result
// per message), so a Turn fans out into one or more wire messages exactly the
// way the compat shim's ConversationFromTurns fans a Turn into shim turns.
//
// wireTurn is that message-granular unit. It is deliberately unexported and
// metadata-free: it exists only inside the provider edge, between the neutral
// model and the dialect emitters (claude_chat_native.go /
// openai_chat_native.go). flattenTurns is a faithful port of the carrier
// semantics in turns.go's conversationTurnsFromTurn — same fan-out, same text
// promotion, same canonicalizations (C1/C2) — so the native wire bytes are
// IDENTICAL to the adapter path's for every expressible input. The
// differential tests in chat_native_test.go (fixed corpora + randomized
// arbitrary-shape trials) pin that equivalence against the adapter path.

// wireTurn is one message-granular unit of neutral content, ready for a
// dialect emitter. role mirrors TurnRole values; result is meaningful only
// for role "tool" (a zero ToolResult is a valid degenerate result, matching
// the shim's inline result fields).
type wireTurn struct {
	role      string
	content   string          // promoted leading text (the shim's Content slot)
	thinking  []ThinkingBlock // assistant extended-thinking blocks, replay order
	toolCalls []ToolCall      // assistant tool_use requests
	result    ToolResult      // role "tool": the tool result payload
	blocks    []ContentBlock  // trailing text/image attachments, in block order
}

// flattenTurns converts neutral turns into wire units, fanning out
// multi-result turns. Port of ConversationFromTurns' grouping (turns.go),
// minus the metadata the wire never carries.
func flattenTurns(turns []Turn) []wireTurn {
	var out []wireTurn
	for _, t := range turns {
		out = append(out, flattenTurn(t)...)
	}
	return out
}

// flattenTurn converts one neutral Turn into one or more wire units. It
// mirrors conversationTurnsFromTurn exactly:
//
//   - The first unit keeps the turn's role and always leads.
//   - A text block preceded only by thinking blocks is promoted into content
//     (rule C1); any later text becomes an attachment block.
//   - For role "tool", the first tool_result folds into the first unit; every
//     additional tool_result (or a result on a non-tool role) fans out its own
//     "tool" unit, and following attachments land beside that result.
//   - Image blocks become image attachments; unknown block kinds read as text
//     (rule C2). Nil payload pointers are skipped the same way the shim
//     converter skips them.
func flattenTurn(t Turn) []wireTurn {
	first := &wireTurn{role: string(t.Role)}
	emitted := []*wireTurn{first}
	carrier := first
	firstHasResult := false
	promotable := true

	for _, b := range t.Blocks {
		switch b.Kind {
		case BlockKindThinking:
			if b.Thinking != nil {
				carrier.thinking = append(carrier.thinking, *b.Thinking)
			}
			continue // thinking blocks don't consume the promotion slot
		case BlockKindToolCall:
			promotable = false
			if b.ToolCall != nil {
				carrier.toolCalls = append(carrier.toolCalls, *b.ToolCall)
			}
		case BlockKindToolResult:
			promotable = false
			tr := ToolResult{}
			if b.ToolResult != nil {
				tr = *b.ToolResult
			}
			if t.Role == RoleTool && !firstHasResult && carrier == first {
				// The turn's primary result: fold into the leading unit.
				first.result = tr
				firstHasResult = true
				continue
			}
			// Additional (or out-of-role) result: fan out a tool unit.
			next := &wireTurn{role: "tool", result: tr}
			emitted = append(emitted, next)
			carrier = next
		case BlockKindImage:
			promotable = false
			carrier.blocks = append(carrier.blocks, ContentBlock{
				Type:   "image",
				Source: b.Image,
			})
		default: // BlockKindText (and any future kind, read as text — rule C2)
			if t.Role != RoleTool && promotable {
				first.content = b.Text
				promotable = false
				continue
			}
			promotable = false
			carrier.blocks = append(carrier.blocks, ContentBlock{
				Type: "text",
				Text: b.Text,
			})
		}
	}

	out := make([]wireTurn, len(emitted))
	for i, p := range emitted {
		out[i] = *p
	}
	return out
}
