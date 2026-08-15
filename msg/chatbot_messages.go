// SPDX-License-Identifier: Apache-2.0

package msg

// ---------------------------------------------------------------------------
// Chatbot message tags
// ---------------------------------------------------------------------------

const (
	TagChatbotSessionCreated   = "MsgChatbotSessionCreated"
	TagChatbotSessionClosed    = "MsgChatbotSessionClosed"
	TagChatbotTakeover         = "MsgChatbotTakeover"
	TagChatbotRelease          = "MsgChatbotRelease"
	TagChatbotHumanReply       = "MsgChatbotHumanReply"
	TagChatbotVisitorTyping    = "MsgChatbotVisitorTyping"
	TagChatbotSessionList      = "MsgChatbotSessionList"
	TagChatbotSessionListReply = "MsgChatbotSessionListReply"
)

// ---------------------------------------------------------------------------
// Chatbot Messages
// ---------------------------------------------------------------------------

// MsgChatbotSessionCreated is published when a new visitor chat session begins.
type MsgChatbotSessionCreated struct {
	ConfigID    string `json:"config_id"`
	SessionID   string `json:"session_id"`
	PaneID      string `json:"pane_id"`
	VisitorID   string `json:"visitor_id"`
	VisitorName string `json:"visitor_name"`
	OriginURL   string `json:"origin_url"`
}

// MsgChatbotSessionClosed is published when a chat session ends.
type MsgChatbotSessionClosed struct {
	ConfigID  string `json:"config_id"`
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"`
	Reason    string `json:"reason"` // "visitor_left", "timeout", "operator_closed"
}

// MsgChatbotTakeover is sent when a human operator takes over a session from the bot.
type MsgChatbotTakeover struct {
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
}

// MsgChatbotRelease is sent when a human operator releases a session back to the bot.
type MsgChatbotRelease struct {
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"`
	UserID    string `json:"user_id"`
}

// MsgChatbotHumanReply is sent when a human operator sends a message to the visitor.
type MsgChatbotHumanReply struct {
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"`
	Content   string `json:"content"`
	UserName  string `json:"user_name"`
}

// MsgChatbotVisitorTyping indicates whether the visitor is currently typing.
type MsgChatbotVisitorTyping struct {
	SessionID string `json:"session_id"`
	IsTyping  bool   `json:"is_typing"`
}

// MsgChatbotSessionList requests a list of active sessions for a config.
type MsgChatbotSessionList struct {
	ConfigID string `json:"config_id"`
}

// MsgChatbotSessionListReply is the response containing active session info.
type MsgChatbotSessionListReply struct {
	Sessions []ChatbotSessionInfo `json:"sessions"`
}

// ChatbotSessionInfo describes a single active chatbot session.
type ChatbotSessionInfo struct {
	SessionID    string `json:"session_id"`
	PaneID       string `json:"pane_id"`
	VisitorID    string `json:"visitor_id"`
	VisitorName  string `json:"visitor_name"`
	OriginURL    string `json:"origin_url"`
	Status       string `json:"status"`
	TakenOverBy  string `json:"taken_over_by"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at"`
	LastMessage  string `json:"last_message"`
}

// RegisterChatbotCodecs registers all chatbot message types with the given codec registry.
func RegisterChatbotCodecs(r *CodecRegistry) {
	r.Register(TagChatbotSessionCreated, "*msg.MsgChatbotSessionCreated", jsonDecoder[MsgChatbotSessionCreated]())
	r.Register(TagChatbotSessionClosed, "*msg.MsgChatbotSessionClosed", jsonDecoder[MsgChatbotSessionClosed]())
	r.Register(TagChatbotTakeover, "*msg.MsgChatbotTakeover", jsonDecoder[MsgChatbotTakeover]())
	r.Register(TagChatbotRelease, "*msg.MsgChatbotRelease", jsonDecoder[MsgChatbotRelease]())
	r.Register(TagChatbotHumanReply, "*msg.MsgChatbotHumanReply", jsonDecoder[MsgChatbotHumanReply]())
	r.Register(TagChatbotVisitorTyping, "*msg.MsgChatbotVisitorTyping", jsonDecoder[MsgChatbotVisitorTyping]())
	r.Register(TagChatbotSessionList, "*msg.MsgChatbotSessionList", jsonDecoder[MsgChatbotSessionList]())
	r.Register(TagChatbotSessionListReply, "*msg.MsgChatbotSessionListReply", jsonDecoder[MsgChatbotSessionListReply]())
}
