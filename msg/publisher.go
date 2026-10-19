// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSPublisher publishes typed messages to NATS subjects using NATSEnvelope.
type NATSPublisher struct {
	nc     *nats.Conn
	codecs *CodecRegistry

	// Reply multiplexing: one wildcard subscription on <respPrefix>.* serves
	// every Request, routed to the waiting caller by the last subject token —
	// the scheme nats.go uses for its own requests. Request used to subscribe
	// and unsubscribe a fresh inbox per call: two protocol round trips through
	// the server for every hop of every snapshot cascade (F-71).
	respOnce   sync.Once
	respErr    error
	respPrefix string
	respMu     sync.Mutex
	respNext   uint64
	respWait   map[string]chan *nats.Msg
}

// NewNATSPublisher creates a NATSPublisher backed by nc and codecs.
func NewNATSPublisher(nc *nats.Conn, codecs *CodecRegistry) *NATSPublisher {
	return &NATSPublisher{nc: nc, codecs: codecs}
}

// Codecs returns the CodecRegistry used by this publisher.
func (p *NATSPublisher) Codecs() *CodecRegistry { return p.codecs }

// Send publishes a fire-and-forget typed message to subject.
// The message is serialized as a NATSEnvelope with the TypeTag looked up from codecs.
func (p *NATSPublisher) Send(subject string, message interface{}) error {
	data, err := p.marshal(message, "")
	if err != nil {
		return err
	}
	if ml := MsgLog(); ml.Enabled() {
		tag := p.codecs.TagOf(message)
		ml.LogSend(subject, tag, len(data), subjectActor(subject))
		ml.LogPayload("SEND", subject, tag, data, subjectActor(subject))
	}
	return p.nc.Publish(subject, data)
}

// Request publishes a message and waits up to timeout for a typed reply.
// The reply is decoded via codecs and returned as an interface{}.
func (p *NATSPublisher) Request(subject string, message interface{}, timeout time.Duration) (interface{}, error) {
	// Register for the reply before publishing, so it cannot be missed.
	replyTo, replyCh, release, err := p.awaitReply()
	if err != nil {
		return nil, fmt.Errorf("request: subscribe reply inbox: %w", err)
	}
	defer release()

	data, err := p.marshal(message, replyTo)
	if err != nil {
		return nil, err
	}

	if ml := MsgLog(); ml.Enabled() {
		tag := p.codecs.TagOf(message)
		ml.LogRequest(subject, tag, len(data), timeout, subjectActor(subject))
		ml.LogPayload("REQ", subject, tag, data, subjectActor(subject))
	}

	if err := p.nc.Publish(subject, data); err != nil {
		return nil, fmt.Errorf("request: publish: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case replyMsg := <-replyCh:
		var env NATSEnvelope
		if err := json.Unmarshal(replyMsg.Data, &env); err != nil {
			return nil, fmt.Errorf("request: decode reply envelope: %w", err)
		}
		decoded, err := p.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("request: decode reply message: %w", err)
		}
		if ml := MsgLog(); ml.Enabled() {
			ml.LogReply(replyTo, env.TypeTag, len(replyMsg.Data), subjectActor(subject))
			ml.LogPayload("REPLY", subject, env.TypeTag, env.Payload, subjectActor(subject))
		}
		return decoded, nil
	case <-timer.C:
		if ml := MsgLog(); ml.Enabled() {
			ml.LogError("request timeout", subject, fmt.Errorf("timeout after %s", timeout), subjectActor(subject))
		}
		return nil, fmt.Errorf("request: timeout waiting for reply on %s", subject)
	}
}

// awaitReply registers a one-shot reply slot on the publisher's shared inbox
// subscription and returns its subject, the channel the reply arrives on, and
// a release func that must be called when the caller stops waiting.
func (p *NATSPublisher) awaitReply() (string, <-chan *nats.Msg, func(), error) {
	p.respOnce.Do(func() {
		p.respPrefix = nats.NewInbox()
		p.respWait = make(map[string]chan *nats.Msg)
		_, p.respErr = p.nc.Subscribe(p.respPrefix+".*", p.routeReply)
	})
	if p.respErr != nil {
		return "", nil, nil, p.respErr
	}
	ch := make(chan *nats.Msg, 1)
	p.respMu.Lock()
	p.respNext++
	token := strconv.FormatUint(p.respNext, 36)
	p.respWait[token] = ch
	p.respMu.Unlock()
	release := func() {
		p.respMu.Lock()
		delete(p.respWait, token)
		p.respMu.Unlock()
	}
	return p.respPrefix + "." + token, ch, release, nil
}

// routeReply hands a reply to the Request waiting on its token. A reply for a
// request that already timed out, or a duplicate, is dropped.
func (p *NATSPublisher) routeReply(m *nats.Msg) {
	token := m.Subject[strings.LastIndexByte(m.Subject, '.')+1:]
	p.respMu.Lock()
	ch := p.respWait[token]
	delete(p.respWait, token)
	p.respMu.Unlock()
	if ch != nil {
		ch <- m // buffered 1, and removed from the map: never blocks
	}
}

// Flush forces an immediate flush of the underlying NATS connection buffer.
func (p *NATSPublisher) Flush() error {
	return p.nc.Flush()
}

// ---------------------------------------------------------------------------
// Per-mode output publishing helpers (deprecated — use SendConversation).
//
// These helpers now internally construct a ConversationMessage and route through
// SendConversation. They are retained for backward compatibility with callers in
// rysh-cli that have not yet migrated to the unified ConversationMessage API.
// ---------------------------------------------------------------------------

// SendPaneOutput publishes text to the pane's merged output subject.
// Deprecated: use SendConversation with an appropriate ConversationMessage.
func (p *NATSPublisher) SendPaneOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceSystem,
		Content:          text,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneShellOutput publishes shell output via ConversationMessage.
// Deprecated: use SendConversation with ConvShell.
func (p *NATSPublisher) SendPaneShellOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceSystem,
		Content:          text,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneAIOutput publishes AI/agentic output via ConversationMessage.
// Deprecated: use SendConversation with ConvAI.
func (p *NATSPublisher) SendPaneAIOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvAI,
		InputType:        InputPrompt,
		MessageSource:    SourceAI,
		Content:          text,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneChatOutput publishes chat output via ConversationMessage.
// Deprecated: use SendConversation with ConvChat.
func (p *NATSPublisher) SendPaneChatOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvChat,
		InputType:        InputMessage,
		MessageSource:    SourceAI,
		Content:          text,
		TimestampMs:      NowMs(),
		SubjectToShare:   true,
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneRyshOutput publishes rysh system command output via ConversationMessage.
// Deprecated: use SendConversation with ConvRysh.
func (p *NATSPublisher) SendPaneRyshOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvRysh,
		InputType:        InputCommand,
		MessageSource:    SourceSystem,
		Content:          text,
		TimestampMs:      NowMs(),
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneExternalOutput publishes external-channel output via ConversationMessage.
// Deprecated: use SendConversation with ConvEmail/ConvSlack/ConvChatbot.
func (p *NATSPublisher) SendPaneExternalOutput(paneID, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConvChatbot, // default to chatbot; callers should migrate
		InputType:        InputMessage,
		MessageSource:    SourceHumanoid,
		Content:          text,
		TimestampMs:      NowMs(),
	}
	return p.SendConversation(paneID, cm)
}

// SendPaneModeOutput publishes output to a named custom mode (e.g. a humanoid's
// own per-humanoid mode), on topic pane.{paneID}.output.{mode}. The mode string
// becomes the ConversationType, so the PaneActor stores it in its dynamic
// per-mode buffer and the TUI renders it under a mode named {mode}. Used for
// per-humanoid modes so each humanoid's channel messages get their own view and
// NATS channel instead of sharing the single "external" buffer.
func (p *NATSPublisher) SendPaneModeOutput(paneID, mode, text string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnAnswer,
		ConversationType: ConversationType(mode),
		InputType:        InputMessage,
		MessageSource:    SourceHumanoid,
		Content:          text,
		TimestampMs:      NowMs(),
	}
	return p.SendConversation(paneID, cm)
}

// ---------------------------------------------------------------------------
// Unified conversation publishing helpers (new — structured messages)
// ---------------------------------------------------------------------------

// SendConversation publishes a ConversationMessage to the appropriate output
// topic(s). For dual-publish modes (shell, ai), it publishes to both the
// per-mode topic and the merged output topic. For other modes, it publishes
// to the per-mode topic only.
func (p *NATSPublisher) SendConversation(paneID string, cm *ConversationMessage) error {
	wrapped := &MsgConversationAppend{Message: cm}
	mode := string(cm.ConversationType)

	// Always publish to per-mode topic.
	_ = p.Send(T("pane", paneID, "output", mode), wrapped)

	// Dual-publish for shell and ai → merged output topic.
	if cm.ConversationType.IsDualPublish() {
		return p.Send(T("pane", paneID, "output"), wrapped)
	}
	return nil
}

// SendConversationHistory publishes a ConversationMessage to the appropriate
// history topic(s). Follows the same dual-publish rules as SendConversation.
func (p *NATSPublisher) SendConversationHistory(paneID string, cm *ConversationMessage) error {
	wrapped := &MsgConversationHistoryAppend{Message: cm}
	mode := string(cm.ConversationType)

	// Always publish to per-mode topic.
	_ = p.Send(T("pane", paneID, "history", mode), wrapped)

	// Dual-publish for shell and ai → merged history topic.
	if cm.ConversationType.IsDualPublish() {
		return p.Send(T("pane", paneID, "history"), wrapped)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Browser action publishing helpers
// ---------------------------------------------------------------------------

// SendBrowserActionRequest publishes a browser action request to the pane's
// browser request subject. The Chrome extension subscribes to this subject
// and executes the action.
func (p *NATSPublisher) SendBrowserActionRequest(paneID string, req *MsgBrowserActionRequest) error {
	return p.Send(T("pane", paneID, "browser", "request"), req)
}

// ---------------------------------------------------------------------------
// Per-mode history publishing helpers (deprecated — use SendConversationHistory).
// ---------------------------------------------------------------------------

// SendPaneHistory publishes a history entry to the merged history topic.
// Deprecated: use SendConversationHistory.
func (p *NATSPublisher) SendPaneHistory(paneID, entry string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnQuestion,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceHuman,
		Content:          entry,
		TimestampMs:      NowMs(),
	}
	return p.SendConversationHistory(paneID, cm)
}

// SendPaneShellHistory publishes a shell command history entry via ConversationMessage.
// Deprecated: use SendConversationHistory with ConvShell.
func (p *NATSPublisher) SendPaneShellHistory(paneID, entry string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnQuestion,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceHuman,
		Content:          entry,
		TimestampMs:      NowMs(),
	}
	return p.SendConversationHistory(paneID, cm)
}

// SendPaneAIHistory publishes an AI prompt history entry via ConversationMessage.
// Deprecated: use SendConversationHistory with ConvAI.
func (p *NATSPublisher) SendPaneAIHistory(paneID, entry string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnQuestion,
		ConversationType: ConvAI,
		InputType:        InputPrompt,
		MessageSource:    SourceHuman,
		Content:          entry,
		TimestampMs:      NowMs(),
	}
	return p.SendConversationHistory(paneID, cm)
}

// SendPaneChatHistory publishes a chat message history entry via ConversationMessage.
// Deprecated: use SendConversationHistory with ConvChat.
func (p *NATSPublisher) SendPaneChatHistory(paneID, entry string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnQuestion,
		ConversationType: ConvChat,
		InputType:        InputMessage,
		MessageSource:    SourceHuman,
		Content:          entry,
		TimestampMs:      NowMs(),
	}
	return p.SendConversationHistory(paneID, cm)
}

// SendPaneRyshHistory publishes a rysh command history entry via ConversationMessage.
// Deprecated: use SendConversationHistory with ConvRysh.
func (p *NATSPublisher) SendPaneRyshHistory(paneID, entry string) error {
	cm := &ConversationMessage{
		TurnID:           NewTurnID(),
		TurnType:         TurnQuestion,
		ConversationType: ConvRysh,
		InputType:        InputCommand,
		MessageSource:    SourceHuman,
		Content:          entry,
		TimestampMs:      NowMs(),
	}
	return p.SendConversationHistory(paneID, cm)
}

// ---------------------------------------------------------------------------
// Memory publishing helpers
// ---------------------------------------------------------------------------

// SendMemoryAppend publishes a new memory entry to the pane's memory topic.
func (p *NATSPublisher) SendMemoryAppend(paneID string, entry *MemoryEntry) error {
	return p.Send(T("pane", paneID, "memory", string(entry.Mode)), &MsgMemoryAppend{
		PaneID: paneID,
		Entry:  *entry,
	})
}

// SendMemorySummarize requests that the MemoryManager summarize the given turns.
func (p *NATSPublisher) SendMemorySummarize(paneID string, mode ConversationType, turns []ConversationMessage) error {
	return p.Send(T("pane", paneID, "memory", "summarize"), &MsgMemorySummarize{
		PaneID: paneID,
		Mode:   mode,
		Turns:  turns,
	})
}

// marshal serializes message into a NATSEnvelope JSON payload.
func (p *NATSPublisher) marshal(message interface{}, replyTo string) ([]byte, error) {
	tag := p.codecs.TagOf(message)
	if tag == "" {
		return nil, fmt.Errorf("publisher: unregistered message type %T", message)
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("publisher: marshal inner message: %w", err)
	}
	env := NATSEnvelope{
		TypeTag: tag,
		ReplyTo: replyTo,
		Payload: payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("publisher: marshal envelope: %w", err)
	}
	return data, nil
}

// subjectActor extracts a short actor identifier from a NATS subject.
// All pane-level subjects resolve to "pane:<shortID>" so every log entry
// from the same pane shares one identifier regardless of sub-actor.
func subjectActor(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 || parts[0] != SessionPrefix() {
		return subject
	}
	kind := parts[1] // "pane", "ws", "tab", "pane-group"
	id := parts[2]
	if len(id) > 8 {
		id = id[:8]
	}
	return kind + ":" + id
}
