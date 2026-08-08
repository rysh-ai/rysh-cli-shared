package msg

import (
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// TypeTag constants for output/status/pipeline messages.
// Agentic tags are defined in agentic_messages.go.
// ---------------------------------------------------------------------------

const (
	TagPaneOutputAppend         = "MsgPaneOutputAppend"
	TagPaneShellOutputAppend    = "MsgPaneShellOutputAppend"
	TagPaneAIOutputAppend       = "MsgPaneAIOutputAppend"
	TagPaneChatOutputAppend     = "MsgPaneChatOutputAppend"
	TagPaneRyshOutputAppend     = "MsgPaneRyshOutputAppend"
	TagPaneExternalOutputAppend = "MsgPaneExternalOutputAppend"
	TagPaneStatusUpdate         = "MsgPaneStatusUpdate"
	TagPipelineOutputAppend     = "MsgPipelineOutputAppend"

	// History tags
	TagPaneHistoryAppend         = "MsgPaneHistoryAppend"
	TagPaneShellHistoryAppend    = "MsgPaneShellHistoryAppend"
	TagPaneAIHistoryAppend       = "MsgPaneAIHistoryAppend"
	TagPaneChatHistoryAppend     = "MsgPaneChatHistoryAppend"
	TagPaneRyshHistoryAppend     = "MsgPaneRyshHistoryAppend"
	TagPaneExternalHistoryAppend = "MsgPaneExternalHistoryAppend"

	// Unified conversation tags (new — replaces the per-mode tags above).
	TagConversationAppend        = "MsgConversationAppend"
	TagConversationHistoryAppend = "MsgConversationHistoryAppend"
)

// ---------------------------------------------------------------------------
// NATSEnvelope — wire format for all inter-actor messages.
// ---------------------------------------------------------------------------

// NATSEnvelope is the JSON-encoded wrapper for all messages published to NATS.
//
// Payload is a json.RawMessage so the already-JSON-encoded inner message is
// embedded verbatim into the envelope. With a plain []byte, encoding/json
// base64-encodes the payload (inflating size ~33% and costing extra CPU on
// every message); json.RawMessage avoids that round-trip entirely.
//
// NOTE: this changes the on-the-wire envelope format. All clients that
// exchange envelopes (including remote sharing via the upstream server) must
// run a build with this change.
type NATSEnvelope struct {
	TypeTag string          `json:"t"` // message type discriminator string constant
	ReplyTo string          `json:"r"` // NATS reply subject; empty for fire-and-forget
	Payload json.RawMessage `json:"p"` // JSON inner message, embedded verbatim
}

// ---------------------------------------------------------------------------
// RequestEnvelope — wraps a decoded message when ReplyTo is set.
// ---------------------------------------------------------------------------

// RequestEnvelope is delivered to an actor's mailbox when the sender expects
// a reply. The actor calls env.Reply(responseMsg) to send the response.
type RequestEnvelope struct {
	Inner   interface{}
	ReplyTo string
	NC      *nats.Conn
	Codecs  *CodecRegistry
}

// Reply serializes responseMsg as a NATSEnvelope and publishes it to ReplyTo.
func (r *RequestEnvelope) Reply(responseMsg interface{}) error {
	tag := r.Codecs.TagOf(responseMsg)
	if tag == "" {
		return fmt.Errorf("reply: unknown message type %T", responseMsg)
	}
	payload, err := json.Marshal(responseMsg)
	if err != nil {
		return fmt.Errorf("reply marshal: %w", err)
	}
	env := NATSEnvelope{
		TypeTag: tag,
		Payload: payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("reply envelope marshal: %w", err)
	}
	if ml := MsgLog(); ml.Enabled() {
		ml.LogReply(r.ReplyTo, tag, len(data), r.ReplyTo)
		ml.LogPayload("REPLY", r.ReplyTo, tag, payload, r.ReplyTo)
	}
	return r.NC.Publish(r.ReplyTo, data)
}

// ---------------------------------------------------------------------------
// CodecRegistry
// ---------------------------------------------------------------------------

// CodecRegistry maps TypeTag strings to decode functions and back.
type CodecRegistry struct {
	byTag  map[string]func([]byte) (interface{}, error)
	byType map[reflect_type]string // Go type → TypeTag
}

// reflect_type is a comparable type key based on Go type pointer identity.
type reflect_type struct{ name string }

// NewCodecRegistry creates an empty CodecRegistry.
func NewCodecRegistry() *CodecRegistry {
	return &CodecRegistry{
		byTag:  make(map[string]func([]byte) (interface{}, error)),
		byType: make(map[reflect_type]string),
	}
}

// Register registers a message type with a TypeTag and a decode function.
// typeName must be the Go type string (e.g. "*msg.MsgAgenticPrompt").
// decode must unmarshal payload and return a pointer to the message.
func (r *CodecRegistry) Register(tag string, typeName string, decode func([]byte) (interface{}, error)) {
	r.byTag[tag] = decode
	r.byType[reflect_type{typeName}] = tag
}

// Decode looks up the decode function for tag and calls it with payload.
func (r *CodecRegistry) Decode(tag string, payload []byte) (interface{}, error) {
	fn, ok := r.byTag[tag]
	if !ok {
		return nil, fmt.Errorf("codec: unknown TypeTag %q", tag)
	}
	return fn(payload)
}

// TagOf returns the TypeTag for the given message (by Go type name).
// Returns "" if the type is not registered.
func (r *CodecRegistry) TagOf(msg interface{}) string {
	if msg == nil {
		return ""
	}
	typeName := fmt.Sprintf("%T", msg)
	return r.byType[reflect_type{typeName}]
}

// helper to build a typed JSON decoder.
func jsonDecoder[T any]() func([]byte) (interface{}, error) {
	return func(b []byte) (interface{}, error) {
		v := new(T)
		if len(b) > 0 {
			if err := json.Unmarshal(b, v); err != nil {
				return nil, err
			}
		}
		return v, nil
	}
}

// DefaultCodecRegistry returns a CodecRegistry pre-registered with the
// agentic and output/status message types needed by the shared module.
func DefaultCodecRegistry() *CodecRegistry {
	r := NewCodecRegistry()

	// Output messages
	r.Register(TagPaneOutputAppend, "*msg.MsgPaneOutputAppend", jsonDecoder[MsgPaneOutputAppend]())
	r.Register(TagPaneShellOutputAppend, "*msg.MsgPaneShellOutputAppend", jsonDecoder[MsgPaneShellOutputAppend]())
	r.Register(TagPaneAIOutputAppend, "*msg.MsgPaneAIOutputAppend", jsonDecoder[MsgPaneAIOutputAppend]())
	r.Register(TagPaneChatOutputAppend, "*msg.MsgPaneChatOutputAppend", jsonDecoder[MsgPaneChatOutputAppend]())
	r.Register(TagPaneRyshOutputAppend, "*msg.MsgPaneRyshOutputAppend", jsonDecoder[MsgPaneRyshOutputAppend]())
	r.Register(TagPaneExternalOutputAppend, "*msg.MsgPaneExternalOutputAppend", jsonDecoder[MsgPaneExternalOutputAppend]())
	r.Register(TagPaneStatusUpdate, "*msg.MsgPaneStatusUpdate", jsonDecoder[MsgPaneStatusUpdate]())
	r.Register(TagPipelineOutputAppend, "*msg.MsgPipelineOutputAppend", jsonDecoder[MsgPipelineOutputAppend]())

	// History messages
	r.Register(TagPaneHistoryAppend, "*msg.MsgPaneHistoryAppend", jsonDecoder[MsgPaneHistoryAppend]())
	r.Register(TagPaneShellHistoryAppend, "*msg.MsgPaneShellHistoryAppend", jsonDecoder[MsgPaneShellHistoryAppend]())
	r.Register(TagPaneAIHistoryAppend, "*msg.MsgPaneAIHistoryAppend", jsonDecoder[MsgPaneAIHistoryAppend]())
	r.Register(TagPaneChatHistoryAppend, "*msg.MsgPaneChatHistoryAppend", jsonDecoder[MsgPaneChatHistoryAppend]())
	r.Register(TagPaneRyshHistoryAppend, "*msg.MsgPaneRyshHistoryAppend", jsonDecoder[MsgPaneRyshHistoryAppend]())
	r.Register(TagPaneExternalHistoryAppend, "*msg.MsgPaneExternalHistoryAppend", jsonDecoder[MsgPaneExternalHistoryAppend]())

	// Unified conversation messages (new)
	r.Register(TagConversationAppend, "*msg.MsgConversationAppend", jsonDecoder[MsgConversationAppend]())
	r.Register(TagConversationHistoryAppend, "*msg.MsgConversationHistoryAppend", jsonDecoder[MsgConversationHistoryAppend]())

	// Agentic messages
	RegisterAgenticCodecs(r)

	// Browser action messages
	RegisterBrowserCodecs(r)

	// Chatbot messages
	RegisterChatbotCodecs(r)

	// Memory messages
	registerMemoryCodecs(r)

	// Usage ledger messages (design 003)
	RegisterUsageCodecs(r)

	// Governance-proxy audit plane (design 001 §4.5)
	RegisterProxyAuditCodecs(r)

	// LLM gateway control plane (design 023)
	RegisterGatewayCodecs(r)

	return r
}
