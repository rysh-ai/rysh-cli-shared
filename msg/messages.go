// Package msg contains the minimal message types shared between the agentic
// layer and the transport layer. This is a subset of the full CLI message set —
// only the types needed by NATSPublisher helpers and the agentic actors.
//
// The canonical message format is ConversationMessage (see conversation.go).
// The per-mode output/history types below are deprecated and retained only for
// backward compatibility with rysh-cli actors that haven't migrated yet.
// New code should use MsgConversationAppend and MsgConversationHistoryAppend.
package msg

// ---------------------------------------------------------------------------
// Output messages — DEPRECATED. Use MsgConversationAppend instead.
// These types are kept for rysh-cli codec compatibility during migration.
// ---------------------------------------------------------------------------

// Deprecated: use MsgConversationAppend with ConvShell.
type MsgPaneOutputAppend struct {
	Text string `json:"text"`
}

// Deprecated: use MsgConversationAppend with ConvShell.
type MsgPaneShellOutputAppend struct {
	Text string `json:"text"`
}

// Deprecated: use MsgConversationAppend with ConvAI.
type MsgPaneAIOutputAppend struct {
	Text string `json:"text"`
}

// Deprecated: use MsgConversationAppend with ConvChat.
type MsgPaneChatOutputAppend struct {
	Text string `json:"text"`
	Role string `json:"role,omitempty"`
}

// Deprecated: use MsgConversationAppend with ConvRysh.
type MsgPaneRyshOutputAppend struct {
	Text string `json:"text"`
}

// Deprecated: use MsgConversationAppend with ConvEmail/ConvSlack/ConvChatbot.
type MsgPaneExternalOutputAppend struct {
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// History messages — DEPRECATED. Use MsgConversationHistoryAppend instead.
// ---------------------------------------------------------------------------

// Deprecated: use MsgConversationHistoryAppend with ConvShell.
type MsgPaneHistoryAppend struct {
	Entry string `json:"entry"`
}

// Deprecated: use MsgConversationHistoryAppend with ConvShell.
type MsgPaneShellHistoryAppend struct {
	Entry string `json:"entry"`
}

// Deprecated: use MsgConversationHistoryAppend with ConvAI.
type MsgPaneAIHistoryAppend struct {
	Entry string `json:"entry"`
}

// Deprecated: use MsgConversationHistoryAppend with ConvChat.
type MsgPaneChatHistoryAppend struct {
	Entry string `json:"entry"`
}

// Deprecated: use MsgConversationHistoryAppend with ConvRysh.
type MsgPaneRyshHistoryAppend struct {
	Entry string `json:"entry"`
}

// Deprecated: use MsgConversationHistoryAppend with ConvEmail/ConvSlack/ConvChatbot.
type MsgPaneExternalHistoryAppend struct {
	Entry string `json:"entry"`
}

// ---------------------------------------------------------------------------
// Status and pipeline messages.
// ---------------------------------------------------------------------------

// MsgPaneStatusUpdate updates the status displayed for the pane.
type MsgPaneStatusUpdate struct {
	Status string `json:"status"`
}

// MsgPipelineOutputAppend is published to a pipeline-level output topic.
type MsgPipelineOutputAppend struct {
	Text string `json:"text"`
}
