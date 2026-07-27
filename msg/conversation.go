// Package msg — conversation.go defines the unified ConversationMessage type
// and related enums. This is the canonical Go representation used by all actors.
//
// JSON is used as the wire format on the NATS bus. The protobuf definitions
// in rysh-proto/ exist as reference schemas but are not used for serialization.
package msg

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// ConversationType enumerates all pane conversation modes.
type ConversationType string

const (
	ConvShell   ConversationType = "shell"
	ConvAI      ConversationType = "ai"
	ConvRysh    ConversationType = "rysh"
	ConvChat    ConversationType = "chat"
	ConvEmail   ConversationType = "email"
	ConvSlack   ConversationType = "slack"
	ConvChatbot ConversationType = "chatbot"
)

// IsDualPublish returns true if this conversation type dual-publishes to
// both its per-mode topic and the merged output topic.
func (ct ConversationType) IsDualPublish() bool {
	return ct == ConvShell || ct == ConvAI
}

// HasMemory returns true if this conversation type supports memory
// summarization (LLM-curated summaries of older turns).
func (ct ConversationType) HasMemory() bool {
	switch ct {
	case ConvAI, ConvEmail, ConvSlack, ConvChatbot:
		return true
	}
	return false
}

// TurnType distinguishes the question from the answer within a turn.
type TurnType string

const (
	TurnQuestion TurnType = "question"
	TurnAnswer   TurnType = "answer"
)

// InputType classifies how the input was entered.
type InputType string

const (
	InputShell    InputType = "shell"
	InputPrompt   InputType = "prompt"
	InputCommand  InputType = "command"
	InputApproval InputType = "approval"
	InputMessage  InputType = "message"
)

// MessageSource identifies who produced the message.
type MessageSource string

const (
	SourceHuman    MessageSource = "human"
	SourceAI       MessageSource = "ai"
	SourceExternal MessageSource = "external"
	SourceAgent    MessageSource = "agent"
	SourceSubagent MessageSource = "subagent"
	SourceHumanoid MessageSource = "humanoid"
	SourceSystem   MessageSource = "system"
)

// ---------------------------------------------------------------------------
// ConversationMessage — the unified message type for all pane modes.
// ---------------------------------------------------------------------------

// ConversationMessage is the universal message format for all pane modes.
// It replaces the 12 per-mode output/history message types with a single
// struct that carries structured metadata.
type ConversationMessage struct {
	// Identity — links question and answer within a turn.
	TurnID           string           `json:"turn_id"`
	TurnType         TurnType         `json:"turn_type"`
	ConversationType ConversationType `json:"conversation_type"`

	// Source — who created this message and how.
	InputType     InputType     `json:"input_type"`
	MessageSource MessageSource `json:"message_source"`

	// Payload
	Content     string `json:"content"`
	TimestampMs int64  `json:"timestamp_ms"`

	// Metadata
	Sensitive      bool   `json:"sensitive,omitempty"`
	SubjectToShare bool   `json:"subject_to_share,omitempty"`
	Role           string `json:"role,omitempty"`      // multi-party: visitor, assistant, human, system
	Streaming      bool   `json:"streaming,omitempty"` // true = partial chunk of an in-progress answer

	// Origin records mirroring provenance: set when this message is a remote
	// tab/pane's output being mirrored into a local (subscriber) pane, so the
	// content is traceable back to its source. Nil for normal local messages.
	Origin *MessageOrigin `json:"origin,omitempty"`
}

// MessageOrigin describes where a mirrored message came from and which local
// (mirror) pane is reflecting it. Used by tab mirroring so output shown in a
// subscriber pane is traceable to its source tab/pane (and so downstream
// consumers — ##pane listen / ##pipe — can attribute it).
type MessageOrigin struct {
	SourceShareID   string `json:"source_share_id,omitempty"`
	SourceTabID     string `json:"source_tab_id,omitempty"`
	SourcePaneID    string `json:"source_pane_id,omitempty"`
	SourcePaneAlias string `json:"source_pane_alias,omitempty"`
	MirrorTabID     string `json:"mirror_tab_id,omitempty"`
	MirrorPaneID    string `json:"mirror_pane_id,omitempty"`
}

// NewTurnID generates a new UUID for linking a question/answer pair.
func NewTurnID() string {
	return uuid.New().String()
}

// NowMs returns the current time as Unix milliseconds.
func NowMs() int64 {
	return time.Now().UnixMilli()
}

// ---------------------------------------------------------------------------
// Convenience constructors for common patterns.
// ---------------------------------------------------------------------------

// NewShellQuestion creates a ConversationMessage for a shell command (question).
func NewShellQuestion(turnID, command string) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnQuestion,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceHuman,
		Content:          command,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
}

// NewShellAnswer creates a ConversationMessage for shell PTY output (answer).
func NewShellAnswer(turnID, output string) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnAnswer,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceSystem,
		Content:          output,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
}

// NewAIQuestion creates a ConversationMessage for an AI prompt (question).
func NewAIQuestion(turnID, prompt string) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnQuestion,
		ConversationType: ConvAI,
		InputType:        InputPrompt,
		MessageSource:    SourceHuman,
		Content:          prompt,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
}

// NewAIAnswer creates a ConversationMessage for AI/agentic output (answer).
func NewAIAnswer(turnID, output string, streaming bool) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnAnswer,
		ConversationType: ConvAI,
		InputType:        InputPrompt,
		MessageSource:    SourceAI,
		Content:          output,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
		Streaming:        streaming,
	}
}

// NewRyshQuestion creates a ConversationMessage for a rysh system command (question).
func NewRyshQuestion(turnID, command string) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnQuestion,
		ConversationType: ConvRysh,
		InputType:        InputCommand,
		MessageSource:    SourceHuman,
		Content:          command,
		TimestampMs:      NowMs(),
	}
}

// NewRyshAnswer creates a ConversationMessage for rysh command output (answer).
func NewRyshAnswer(turnID, output string) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         TurnAnswer,
		ConversationType: ConvRysh,
		InputType:        InputCommand,
		MessageSource:    SourceSystem,
		Content:          output,
		TimestampMs:      NowMs(),
	}
}

// NewChatMessage creates a ConversationMessage for a chat message.
func NewChatMessage(turnID string, turnType TurnType, content, role string, source MessageSource) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         turnType,
		ConversationType: ConvChat,
		InputType:        InputMessage,
		MessageSource:    source,
		Content:          content,
		TimestampMs:      NowMs(),
		Role:             role,
		SubjectToShare:   true,
	}
}

// NewChannelMessage creates a ConversationMessage for a humanoid channel
// (email, slack, chatbot) message.
func NewChannelMessage(turnID string, turnType TurnType, convType ConversationType,
	content, role string, source MessageSource, sensitive bool) *ConversationMessage {
	return &ConversationMessage{
		TurnID:           turnID,
		TurnType:         turnType,
		ConversationType: convType,
		InputType:        InputMessage,
		MessageSource:    source,
		Content:          content,
		TimestampMs:      NowMs(),
		Role:             role,
		Sensitive:        sensitive,
	}
}

// ---------------------------------------------------------------------------
// New unified message wrappers (wire-format types).
// ---------------------------------------------------------------------------

// MsgConversationAppend publishes a ConversationMessage to a pane output topic.
// Replaces all 6 per-mode output message types.
type MsgConversationAppend struct {
	Message *ConversationMessage `json:"message"`
}

// MsgConversationHistoryAppend publishes a ConversationMessage to a pane history topic.
// Replaces all 6 per-mode history message types.
type MsgConversationHistoryAppend struct {
	Message *ConversationMessage `json:"message"`
}
