package provider

// Design 002 A1 — the neutral conversation model.
//
// Turn / Block / ToolCall / ToolResult are the provider-independent
// representation of a conversation: an ordered list of typed blocks per turn,
// with no Anthropic Messages-API field names in the Go surface. Provider
// dialects (Anthropic content blocks, OpenAI chat messages, …) are produced
// by adapters at the provider edge — see chat_provider.go.
//
// ConversationTurn (agentic_provider.go) remains the compatibility shim: it
// is what the orchestrator stores, what session KV persists, and what the
// AgenticProvider interface accepts. The converters in this file translate
// between the two shapes LOSSLESSLY for every shape ConversationTurn can
// express today; the few degenerate shapes that cannot round-trip exactly are
// canonicalized per the documented rules below (each covered by a test in
// turns_test.go — no silent drops).
//
// Canonicalization rules (ConversationTurn -> Turn -> ConversationTurn):
//
//  (C1) A user/assistant turn whose Content is EMPTY but whose ContentBlocks
//       begin with a text block comes back with that first text block promoted
//       into Content (and removed from ContentBlocks). The Anthropic adapter
//       emits the same wire content either way (Content is prepended as a
//       leading text block when ContentBlocks are present), except in the
//       single-text-block case where the string form replaces the equivalent
//       one-element array form. No known producer creates this shape:
//       Content-plus-image producers always set Content, and the image-KV
//       restorer only substitutes text INTO an image slot alongside a prompt.
//
//  (C2) A ContentBlock with an unrecognized Type (not "text"/"image") comes
//       back as a TEXT ContentBlock carrying its Text; an attached Source is
//       dropped. This mirrors the Anthropic adapter's wire behavior
//       (contentBlockFromConvBlock renders unknown types as text blocks).
//
// Turn -> ConversationTurn for turns NOT produced by TurnsFromConversation
// (e.g. built by a future direct ChatProvider caller) is total, with fan-out:
// every tool_result block yields its own tool-role ConversationTurn (matching
// the one-result-per-turn shim invariant); text/image blocks following a
// tool_result attach to that result's turn as ContentBlocks.

import "encoding/json"

// TurnRole identifies the speaker of a neutral Turn. The underlying string
// values match ConversationTurn.Role so conversion never invents roles;
// unknown roles pass through verbatim (the shim ignores them on the wire,
// and losing them here would break round-tripping).
type TurnRole string

const (
	RoleUser      TurnRole = "user"
	RoleAssistant TurnRole = "assistant"
	RoleTool      TurnRole = "tool"
)

// BlockKind discriminates the union carried by Block.
type BlockKind string

const (
	BlockKindText       BlockKind = "text"
	BlockKindThinking   BlockKind = "thinking"
	BlockKindToolCall   BlockKind = "tool_call"
	BlockKindToolResult BlockKind = "tool_result"
	BlockKindImage      BlockKind = "image"
)

// Turn is one neutral conversation turn: a role plus ordered content blocks.
// The metadata fields (Category/Origin/Summary/TimestampMs) are rysh's own
// turn bookkeeping — provider-independent, carried through conversion
// verbatim. JSON tags are chosen for stability of any FUTURE persistence of
// neutral turns; nothing persists Turn today (session KV stores
// ConversationTurn), and the tags deliberately do not mirror any provider's
// wire format.
type Turn struct {
	Role   TurnRole `json:"role"`
	Blocks []Block  `json:"blocks,omitempty"`

	// Category classifies the turn's origin (TurnCategory* constants).
	Category string `json:"category,omitempty"`
	// Origin identifies the producer within the category.
	Origin string `json:"origin,omitempty"`
	// Summary is a short human-readable digest of the turn.
	Summary string `json:"summary,omitempty"`
	// TimestampMs records when the turn was appended (unix milliseconds).
	TimestampMs int64 `json:"timestamp_ms,omitempty"`
}

// Block is one ordered content element of a Turn. Kind selects which payload
// field is meaningful; the others are zero.
type Block struct {
	Kind BlockKind `json:"kind"`

	// Text carries BlockKindText content.
	Text string `json:"text,omitempty"`
	// Thinking carries BlockKindThinking content (hidden reasoning + the
	// opaque provenance signature some providers require on replay).
	Thinking *ThinkingBlock `json:"thinking,omitempty"`
	// ToolCall carries BlockKindToolCall content.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// ToolResult carries BlockKindToolResult content.
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	// Image carries BlockKindImage content. May be nil for a degenerate
	// image block with no source (preserved as-is for round-tripping).
	Image *ImageSource `json:"image,omitempty"`
}

// ToolCall is a tool invocation requested by the model, in neutral shape.
type ToolCall struct {
	// ID is the provider-assigned call identifier; the matching ToolResult
	// echoes it as CallID.
	ID string `json:"id,omitempty"`
	// Name is the tool name.
	Name string `json:"name,omitempty"`
	// ParamsJSON is the raw JSON arguments object.
	ParamsJSON json.RawMessage `json:"params_json,omitempty"`
}

// ToolResult is the outcome of one tool call, in neutral shape.
type ToolResult struct {
	// CallID references the ToolCall.ID this result answers.
	CallID string `json:"call_id,omitempty"`
	// Content is the textual result payload.
	Content string `json:"content,omitempty"`
	// IsError marks the result as a failure the model should read as such.
	IsError bool `json:"is_error,omitempty"`
}

// ---------------------------------------------------------------------------
// ConversationTurn -> Turn
// ---------------------------------------------------------------------------

// TurnsFromConversation converts a shim conversation into neutral turns, 1:1.
func TurnsFromConversation(conv []ConversationTurn) []Turn {
	if conv == nil {
		return nil
	}
	out := make([]Turn, len(conv))
	for i, ct := range conv {
		out[i] = TurnFromConversation(ct)
	}
	return out
}

// TurnFromConversation converts one shim turn into a neutral Turn.
//
// Block order is canonical replay order: thinking blocks first, then the
// turn's text, then tool calls (assistant) / the tool result (tool role),
// then ContentBlocks (attachments). For tool-role turns, Content is the tool
// RESULT payload and lands inside the ToolResult block, not in a text block.
func TurnFromConversation(ct ConversationTurn) Turn {
	t := Turn{
		Role:        TurnRole(ct.Role),
		Category:    ct.Category,
		Origin:      ct.Origin,
		Summary:     ct.Summary,
		TimestampMs: ct.TimestampMs,
	}

	for i := range ct.Thinking {
		tb := ct.Thinking[i]
		t.Blocks = append(t.Blocks, Block{Kind: BlockKindThinking, Thinking: &tb})
	}

	if ct.Role == "tool" {
		t.Blocks = append(t.Blocks, Block{Kind: BlockKindToolResult, ToolResult: &ToolResult{
			CallID:  ct.ToolCallID,
			Content: ct.Content,
			IsError: ct.IsError,
		}})
	} else if ct.Content != "" {
		t.Blocks = append(t.Blocks, Block{Kind: BlockKindText, Text: ct.Content})
	}

	for _, tc := range ct.ToolCalls {
		t.Blocks = append(t.Blocks, Block{Kind: BlockKindToolCall, ToolCall: &ToolCall{
			ID:         tc.ID,
			Name:       tc.Name,
			ParamsJSON: tc.Input,
		}})
	}

	for _, cb := range ct.ContentBlocks {
		t.Blocks = append(t.Blocks, blockFromContentBlock(cb))
	}

	return t
}

// blockFromContentBlock maps a shim ContentBlock to a neutral Block.
// Unknown types canonicalize to text (rule C2), mirroring the Anthropic
// adapter's wire behavior.
func blockFromContentBlock(cb ContentBlock) Block {
	if cb.Type == "image" {
		return Block{Kind: BlockKindImage, Image: cb.Source}
	}
	return Block{Kind: BlockKindText, Text: cb.Text}
}

// ---------------------------------------------------------------------------
// Turn -> ConversationTurn
// ---------------------------------------------------------------------------

// ConversationFromTurns converts neutral turns back into the shim shape.
//
// Turns produced by TurnsFromConversation come back 1:1 (exactly equal up to
// the documented canonicalizations C1/C2). Arbitrary neutral turns are
// converted totally: each tool_result block yields its own tool-role
// ConversationTurn — a turn carrying several results fans out into several
// shim turns, preserving order — and text/image blocks that FOLLOW a
// tool_result attach to that result's turn as ContentBlocks (attachments).
func ConversationFromTurns(turns []Turn) []ConversationTurn {
	if turns == nil {
		return nil
	}
	out := make([]ConversationTurn, 0, len(turns))
	for _, t := range turns {
		out = append(out, conversationTurnsFromTurn(t)...)
	}
	return out
}

// conversationTurnsFromTurn converts one neutral Turn. It returns a slice
// because tool_result blocks fan out one shim turn per result.
func conversationTurnsFromTurn(t Turn) []ConversationTurn {
	first := &ConversationTurn{
		Role:        string(t.Role),
		Category:    t.Category,
		Origin:      t.Origin,
		Summary:     t.Summary,
		TimestampMs: t.TimestampMs,
	}

	// emitted accumulates the shim turns in order; first is always emitted
	// (even when empty of blocks) and always leads.
	emitted := []*ConversationTurn{first}
	// carrier is the shim turn currently accumulating blocks: first until a
	// fanned-out tool result appears, then that result's turn so attachments
	// (text/image blocks) land beside their result.
	carrier := first
	firstHasResult := false
	// promotable: a text block may be promoted into Content only while
	// nothing but thinking blocks precede it — the exact position
	// TurnFromConversation emits Content at. A text block after any other
	// block kind came from ContentBlocks and stays there.
	promotable := true

	for _, b := range t.Blocks {
		switch b.Kind {
		case BlockKindThinking:
			if b.Thinking != nil {
				carrier.Thinking = append(carrier.Thinking, *b.Thinking)
			}
			continue // thinking blocks don't consume the promotion slot
		case BlockKindToolCall:
			promotable = false
			if b.ToolCall != nil {
				carrier.ToolCalls = append(carrier.ToolCalls, ToolCallRequest{
					ID:    b.ToolCall.ID,
					Name:  b.ToolCall.Name,
					Input: b.ToolCall.ParamsJSON,
				})
			}
		case BlockKindToolResult:
			promotable = false
			tr := ToolResult{}
			if b.ToolResult != nil {
				tr = *b.ToolResult
			}
			if t.Role == RoleTool && !firstHasResult && carrier == first {
				// The turn's primary result: fold into the first shim turn.
				first.ToolCallID = tr.CallID
				first.Content = tr.Content
				first.IsError = tr.IsError
				firstHasResult = true
				continue
			}
			// Additional (or out-of-role) result: fan out a tool turn.
			next := &ConversationTurn{
				Role:        "tool",
				ToolCallID:  tr.CallID,
				Content:     tr.Content,
				IsError:     tr.IsError,
				Category:    t.Category,
				Origin:      t.Origin,
				Summary:     t.Summary,
				TimestampMs: t.TimestampMs,
			}
			emitted = append(emitted, next)
			carrier = next
		case BlockKindImage:
			promotable = false
			carrier.ContentBlocks = append(carrier.ContentBlocks, ContentBlock{
				Type:   "image",
				Source: b.Image,
			})
		default: // BlockKindText (and any future kind, read as text)
			if t.Role != RoleTool && promotable {
				// A text block preceded only by thinking blocks is the turn's
				// Content (the inverse of TurnFromConversation's emission
				// order; see rule C1 for the one degenerate shape this
				// canonicalizes).
				first.Content = b.Text
				promotable = false
				continue
			}
			promotable = false
			carrier.ContentBlocks = append(carrier.ContentBlocks, ContentBlock{
				Type: "text",
				Text: b.Text,
			})
		}
	}

	out := make([]ConversationTurn, len(emitted))
	for i, p := range emitted {
		out[i] = *p
	}
	return out
}
