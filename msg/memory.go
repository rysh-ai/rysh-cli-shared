package msg

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Memory types — curated summaries of older conversation turns.
// These mirror the protobuf definitions in rysh-proto/memorypb as plain
// Go structs with JSON serialization (the canonical wire format).
// ---------------------------------------------------------------------------

// MemoryEntry is a single curated summary covering a batch of older turns.
type MemoryEntry struct {
	ID          string           `json:"id"`            // UUID
	Summary     string           `json:"summary"`       // LLM-generated summary
	TurnIDs     []string         `json:"turn_ids"`      // turn_ids that were summarized
	Mode        ConversationType `json:"mode"`          // conversation mode
	CreatedAtMs int64            `json:"created_at_ms"` // when the memory was created
	TurnCount   int              `json:"turn_count"`    // how many turns were condensed
}

// MemoryState holds the full memory for a pane+mode combination.
type MemoryState struct {
	PaneID               string           `json:"pane_id"`
	Mode                 ConversationType `json:"mode"`
	Entries              []MemoryEntry    `json:"entries"`
	TotalSummarizedTurns int              `json:"total_summarized_turns"`
}

// MaxMemoryEntries is the maximum number of memory entries per pane per mode.
// When exceeded, the oldest entries are merged (re-summarized into one).
const MaxMemoryEntries = 50

// NewMemoryEntry creates a MemoryEntry with a fresh UUID and current timestamp.
func NewMemoryEntry(mode ConversationType, summary string, turnIDs []string) *MemoryEntry {
	return &MemoryEntry{
		ID:          uuid.New().String(),
		Summary:     summary,
		TurnIDs:     turnIDs,
		Mode:        mode,
		CreatedAtMs: time.Now().UnixMilli(),
		TurnCount:   len(turnIDs),
	}
}

// ---------------------------------------------------------------------------
// Memory NATS messages
// ---------------------------------------------------------------------------

// MsgMemoryAppend is published when a new memory entry is created.
type MsgMemoryAppend struct {
	PaneID string      `json:"pane_id"`
	Entry  MemoryEntry `json:"entry"`
}

// MsgMemoryGet requests the full memory state for a pane+mode.
type MsgMemoryGet struct {
	PaneID string           `json:"pane_id"`
	Mode   ConversationType `json:"mode"`
}

// MsgMemoryGetReply is the response to MsgMemoryGet.
type MsgMemoryGetReply struct {
	State *MemoryState `json:"state"`
}

// MsgMemorySummarize requests that older turns be summarized into a memory entry.
// This is sent from PaneActor to the MemoryManager when eviction is triggered.
type MsgMemorySummarize struct {
	PaneID string                `json:"pane_id"`
	Mode   ConversationType      `json:"mode"`
	Turns  []ConversationMessage `json:"turns"` // the turns to summarize
}

// MsgMemorySummarizeDone is published when summarization completes.
type MsgMemorySummarizeDone struct {
	PaneID string           `json:"pane_id"`
	Mode   ConversationType `json:"mode"`
	Entry  MemoryEntry      `json:"entry"`
	Error  string           `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Tag constants for codec registration
// ---------------------------------------------------------------------------

const (
	TagMemoryAppend        = "memory.append"
	TagMemoryGet           = "memory.get"
	TagMemoryGetReply      = "memory.get_reply"
	TagMemorySummarize     = "memory.summarize"
	TagMemorySummarizeDone = "memory.summarize_done"
)

// ---------------------------------------------------------------------------
// Memory formatting for LLM context
// ---------------------------------------------------------------------------

// FormatMemoryForPrompt formats memory entries as a context section
// to be prepended to the system prompt before LLM calls.
func FormatMemoryForPrompt(state *MemoryState) string {
	if state == nil || len(state.Entries) == 0 {
		return ""
	}

	var result string
	result = "\n## Conversation Memory\nThe following is a summary of earlier conversation turns:\n\n"

	for i, entry := range state.Entries {
		ago := timeSince(entry.CreatedAtMs)
		result += fmt.Sprintf("### Memory %d (covers %d turns, created %s)\n%s\n\n",
			i+1, entry.TurnCount, ago, entry.Summary)
	}

	result += "## Recent Conversation\n"
	return result
}

// timeSince returns a human-readable time difference from the given timestamp.
func timeSince(ms int64) string {
	t := time.UnixMilli(ms)
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ---------------------------------------------------------------------------
// Summarization prompt template
// ---------------------------------------------------------------------------

// MemorySummarizationPrompt returns the prompt used to ask the LLM to
// summarize a batch of conversation turns into a memory entry.
func MemorySummarizationPrompt(mode ConversationType, turns []ConversationMessage) string {
	var turnsText string
	for _, t := range turns {
		prefix := "[" + string(t.TurnType) + "]"
		if t.Role != "" {
			prefix += " (" + t.Role + ")"
		}
		turnsText += fmt.Sprintf("%s: %s\n---\n", prefix, t.Content)
	}

	return fmt.Sprintf(`You are summarizing a conversation for future context. The conversation took
place in %s mode. Produce a concise summary (max 500 words) that captures:
1. Key decisions made
2. Files modified or discussed
3. Important context that would help a future prompt understand what happened
4. Any unresolved issues or TODOs

Do NOT include raw code blocks unless they are critical to understanding.

Conversation turns to summarize:
%s`, string(mode), turnsText)
}

// ---------------------------------------------------------------------------
// Codec registration helper (called from DefaultCodecRegistry)
// ---------------------------------------------------------------------------

func registerMemoryCodecs(r *CodecRegistry) {
	r.Register(TagMemoryAppend, "*msg.MsgMemoryAppend", jsonDecoder[MsgMemoryAppend]())
	r.Register(TagMemoryGet, "*msg.MsgMemoryGet", jsonDecoder[MsgMemoryGet]())
	r.Register(TagMemoryGetReply, "*msg.MsgMemoryGetReply", jsonDecoder[MsgMemoryGetReply]())
	r.Register(TagMemorySummarize, "*msg.MsgMemorySummarize", jsonDecoder[MsgMemorySummarize]())
	r.Register(TagMemorySummarizeDone, "*msg.MsgMemorySummarizeDone", jsonDecoder[MsgMemorySummarizeDone]())
}
