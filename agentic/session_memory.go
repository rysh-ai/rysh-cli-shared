// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// SessionMemory is the single owner of a user session's conversational state
// for one LLM execution actor (pane, agent, or humanoid). It consolidates
// what used to be scattered across the actor: the categorized conversation
// turns, the pause/resume checkpoint, and throttled KV persistence.
//
// Responsibilities:
//   - Turns: append / replace / bound-and-heal the categorized conversation.
//   - Checkpoint: a paused run (double-Ctrl+C, iteration cap, wall-clock cap)
//     records Paused + PausedReason; "continue" resumes from the same turns.
//   - Persistence: the full state (turns + checkpoint) is written to JetStream
//     KV under "<owner>.llm_session" (wrapper format, versioned) and the bare
//     turns array is mirrored to the legacy "<owner>.llm_conversation" key so
//     pre-existing readers (PaneActor restore) keep working.
//
// SessionMemory is NOT goroutine-safe by design: it is owned by a single
// actor and only touched from that actor's Receive (proto.actor guarantees
// sequential processing).
type SessionMemory struct {
	ownerID string
	kv      nats.KeyValue

	turns        []provider.ConversationTurn
	paused       bool
	pausedReason string

	// groundingOverride is the user's explicit ##grounding mode override for
	// this pane ("" = none; the host default applies). Persisted so the
	// override survives daemon restarts. Deliberately NOT transferred by a
	// ##hop fork — the target pane keeps its own setting.
	groundingOverride string

	lastWrite        time.Time
	minWriteInterval time.Duration
}

// sessionMemoryVersion identifies the persisted wrapper format.
const sessionMemoryVersion = 1

// sessionKVKeySuffix / legacyConversationKVKeySuffix are the KV key suffixes
// (appended to the owner id) for the wrapper format and the legacy bare-array
// format respectively.
const (
	sessionKVKeySuffix            = ".llm_session"
	legacyConversationKVKeySuffix = ".llm_conversation"
)

// SessionMemoryState is the persisted JSON wrapper.
type SessionMemoryState struct {
	Version      int                         `json:"version"`
	Turns        []provider.ConversationTurn `json:"turns"`
	Paused       bool                        `json:"paused,omitempty"`
	PausedReason string                      `json:"paused_reason,omitempty"`
	// GroundingOverride is the user's explicit ##grounding mode override
	// ("" = none). Additive field: wrappers persisted before it existed
	// simply restore with no override.
	GroundingOverride string `json:"grounding_override,omitempty"`
	UpdatedMs         int64  `json:"updated_ms"`
}

// NewSessionMemory creates a SessionMemory for the given owner (pane / agent /
// humanoid id). kv may be nil (no persistence — e.g. tests).
func NewSessionMemory(ownerID string, kv nats.KeyValue) *SessionMemory {
	return &SessionMemory{
		ownerID:          ownerID,
		kv:               kv,
		turns:            make([]provider.ConversationTurn, 0),
		minWriteInterval: 2 * time.Second,
	}
}

// Turns returns the current conversation window. Callers must not mutate the
// returned slice's backing array; use Append/Replace instead.
func (s *SessionMemory) Turns() []provider.ConversationTurn { return s.turns }

// Len returns the number of retained turns.
func (s *SessionMemory) Len() int { return len(s.turns) }

// AppendUserTurn appends a categorized user turn (Category "user") stamped
// with the current time.
func (s *SessionMemory) AppendUserTurn(content string, blocks []provider.ContentBlock) {
	s.turns = append(s.turns, provider.ConversationTurn{
		Role:          "user",
		Content:       content,
		ContentBlocks: blocks,
		Category:      provider.TurnCategoryUser,
		TimestampMs:   time.Now().UnixMilli(),
	})
}

// Append appends an arbitrary turn (already categorized by the producer).
func (s *SessionMemory) Append(t provider.ConversationTurn) {
	if t.TimestampMs == 0 {
		t.TimestampMs = time.Now().UnixMilli()
	}
	if t.Category == "" {
		t.Category = provider.CategorizeTurn(t)
	}
	s.turns = append(s.turns, t)
}

// Replace swaps the full conversation (used when an orchestrator returns its
// merged transcript). Turns missing a category are back-filled from their
// role so pre-categorisation transcripts stay well-formed.
func (s *SessionMemory) Replace(turns []provider.ConversationTurn) {
	for i := range turns {
		if turns[i].Category == "" {
			turns[i].Category = provider.CategorizeTurn(turns[i])
		}
	}
	s.turns = turns
}

// Bound trims the window to the retained turn budget and heals both ends: the
// head never starts with an orphaned tool_result and the tail never ends with
// an unanswered tool_use (synthetic interrupted results are appended for
// those — required for a paused run to resume cleanly).
func (s *SessionMemory) Bound() {
	s.turns = CloseDanglingToolCalls(s.turns, "interrupted — no result recorded")
	s.turns = boundConversation(s.turns)
}

// Pause records the resumable checkpoint: the reason the run stopped. The
// turns already in memory ARE the checkpoint — nothing else is needed to
// resume, because the orchestrator loop is stateless between LLM calls.
func (s *SessionMemory) Pause(reason string) {
	s.paused = true
	s.pausedReason = reason
}

// ClearPause marks the session as no longer paused (a new prompt or an
// explicit continue arrived).
func (s *SessionMemory) ClearPause() {
	s.paused = false
	s.pausedReason = ""
}

// Paused reports whether the session holds a resumable checkpoint, and why.
func (s *SessionMemory) Paused() (bool, string) { return s.paused, s.pausedReason }

// SetGroundingOverride records (or clears, with "") the user's explicit
// ##grounding mode override for this pane. Persisted with the session so it
// survives daemon restarts.
func (s *SessionMemory) SetGroundingOverride(mode string) {
	s.groundingOverride = mode
}

// GroundingOverride returns the persisted ##grounding override ("" = none).
func (s *SessionMemory) GroundingOverride() string { return s.groundingOverride }

// Persist writes the state to KV. Writes are throttled to at most one per
// minWriteInterval unless force is true (shutdown / pause — moments where
// losing state would break resume).
func (s *SessionMemory) Persist(force bool) {
	if s.kv == nil {
		return
	}
	if !force && time.Since(s.lastWrite) < s.minWriteInterval {
		return
	}
	state := SessionMemoryState{
		Version:           sessionMemoryVersion,
		Turns:             s.turns,
		Paused:            s.paused,
		PausedReason:      s.pausedReason,
		GroundingOverride: s.groundingOverride,
		UpdatedMs:         time.Now().UnixMilli(),
	}
	if data, err := json.Marshal(state); err == nil {
		_, _ = s.kv.Put(s.ownerID+sessionKVKeySuffix, data)
	}
	// Mirror the bare turns array to the legacy key so pre-wrapper readers
	// (PaneActor conversation restore) continue to work unchanged.
	if data, err := json.Marshal(s.turns); err == nil {
		_, _ = s.kv.Put(s.ownerID+legacyConversationKVKeySuffix, data)
	}
	s.lastWrite = time.Now()
}

// Restore loads state from KV, preferring the wrapper format and falling back
// to the legacy bare-array key. Returns true when anything was restored.
//
// The wrapper's settings (grounding override) are restored even when it holds
// no turns yet — an override set on a fresh pane must survive a restart. The
// legacy bare-array key is only consulted for turns the wrapper didn't have.
func (s *SessionMemory) Restore() bool {
	if s.kv == nil {
		return false
	}
	restored := false
	wrapperHadTurns := false
	if entry, err := s.kv.Get(s.ownerID + sessionKVKeySuffix); err == nil {
		var state SessionMemoryState
		if json.Unmarshal(entry.Value(), &state) == nil {
			s.groundingOverride = state.GroundingOverride
			if state.GroundingOverride != "" {
				restored = true
			}
			if len(state.Turns) > 0 {
				s.Replace(state.Turns)
				s.paused = state.Paused
				s.pausedReason = state.PausedReason
				wrapperHadTurns = true
				restored = true
			}
		}
	}
	if !wrapperHadTurns {
		if entry, err := s.kv.Get(s.ownerID + legacyConversationKVKeySuffix); err == nil {
			var turns []provider.ConversationTurn
			if json.Unmarshal(entry.Value(), &turns) == nil && len(turns) > 0 {
				s.Replace(turns)
				restored = true
			}
		}
	}
	return restored
}

// CloseDanglingToolCalls heals the TAIL of a conversation: when the last
// assistant turn carries tool_use blocks that have no matching tool_result
// turns (the run was interrupted mid-tool), synthetic error results are
// appended so the Messages API accepts the transcript on resume. The
// synthetic results are categorized "tool" with the interrupted note, so the
// model knows the calls never ran and can re-issue them.
func CloseDanglingToolCalls(turns []provider.ConversationTurn, note string) []provider.ConversationTurn {
	// Find the last assistant turn with tool calls.
	lastAssistant := -1
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			if len(turns[i].ToolCalls) > 0 {
				lastAssistant = i
			}
			break
		}
		if turns[i].Role == "user" {
			break // a user turn after the assistant closes the exchange
		}
	}
	if lastAssistant < 0 {
		return turns
	}

	// Collect the ids already answered after that assistant turn.
	answered := make(map[string]bool)
	for _, t := range turns[lastAssistant+1:] {
		if t.Role == "tool" && t.ToolCallID != "" {
			answered[t.ToolCallID] = true
		}
	}

	now := time.Now().UnixMilli()
	for _, tc := range turns[lastAssistant].ToolCalls {
		if answered[tc.ID] {
			continue
		}
		turns = append(turns, provider.ConversationTurn{
			Role:        "tool",
			Content:     "[error kind=cancelled] " + note,
			ToolCallID:  tc.ID,
			IsError:     true,
			Category:    provider.TurnCategoryTool,
			Origin:      tc.Name,
			Summary:     stepTitle(tc.Name, tc.Input) + " — interrupted",
			TimestampMs: now,
		})
	}
	return turns
}
