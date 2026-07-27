// Package bridge provides the NATSBridge that delivers NATS messages to a
// proto.actor actor mailbox. Each actor creates one NATSBridge and calls
// AddSubject() for each NATS subject it wants to receive messages on.
package bridge

import (
	"encoding/json"
	"fmt"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// NATSBridge subscribes to one or more NATS subjects and forwards decoded
// messages to a proto.actor actor via its mailbox.
//
// For fire-and-forget messages (ReplyTo == ""), the decoded inner message is
// sent directly.
// For request/reply messages (ReplyTo != ""), a *msg.RequestEnvelope is sent
// so the actor can call env.Reply(responseMsg).
type NATSBridge struct {
	nc     *nats.Conn
	pid    *actor.PID
	paneID string // parent pane ID for logging; empty for non-pane actors
	system *actor.ActorSystem
	codecs *msg.CodecRegistry
	subs   []*nats.Subscription
}

// New creates a NATSBridge that delivers to pid via system.
// pid may be nil if SetPID will be called before any messages arrive.
func New(nc *nats.Conn, pid *actor.PID, system *actor.ActorSystem, codecs *msg.CodecRegistry) *NATSBridge {
	return &NATSBridge{
		nc:     nc,
		pid:    pid,
		system: system,
		codecs: codecs,
	}
}

// SetPID updates the target PID. It must be called before AddSubject() if
// the PID was not known at construction time.
func (b *NATSBridge) SetPID(pid *actor.PID) {
	b.pid = pid
}

// SetPaneID associates this bridge with a parent pane ID so that every log
// entry shows "pane:<shortID>" instead of the proto.actor-generated UUID.
// Call this before AddSubject() for it to take effect on all subscriptions.
func (b *NATSBridge) SetPaneID(paneID string) {
	b.paneID = paneID
}

// AddSubject subscribes to a NATS subject and routes incoming messages to the
// actor's mailbox. Call from the actor's *actor.Started handler.
func (b *NATSBridge) AddSubject(subject string) error {
	sub, err := b.nc.Subscribe(subject, func(m *nats.Msg) {
		actorID := b.logActorID()

		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			if ml := msg.MsgLog(); ml.Enabled() {
				ml.LogError("bridge decode envelope", subject, err, actorID)
			}
			return
		}
		decoded, err := b.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			if ml := msg.MsgLog(); ml.Enabled() {
				ml.LogError("bridge decode message", subject, fmt.Errorf("TypeTag=%s: %w", env.TypeTag, err), actorID)
			}
			return
		}
		// Use env.ReplyTo if set; otherwise fall back to the NATS message's
		// built-in Reply field (set automatically by nats.Conn.Request).
		replyTo := env.ReplyTo
		if replyTo == "" {
			replyTo = m.Reply
		}
		isRequest := replyTo != ""

		if ml := msg.MsgLog(); ml.Enabled() {
			ml.LogRecv(subject, env.TypeTag, actorID, isRequest)
		}

		if isRequest {
			// Wrap in RequestEnvelope so the actor can send a reply.
			b.system.Root.Send(b.pid, &msg.RequestEnvelope{
				Inner:   decoded,
				ReplyTo: replyTo,
				NC:      b.nc,
				Codecs:  b.codecs,
			})
		} else {
			b.system.Root.Send(b.pid, decoded)
		}
	})
	if err != nil {
		return fmt.Errorf("bridge: subscribe %q: %w", subject, err)
	}
	b.subs = append(b.subs, sub)
	return nil
}

// logActorID returns the identifier used in log lines.
func (b *NATSBridge) logActorID() string {
	if b.paneID != "" {
		id := b.paneID
		if len(id) > 8 {
			id = id[:8]
		}
		return "pane:" + id
	}
	return b.pid.GetId()
}

// Stop unsubscribes all NATS subscriptions. Call from the actor's
// *actor.Stopping handler before any other cleanup.
func (b *NATSBridge) Stop() {
	for _, sub := range b.subs {
		_ = sub.Unsubscribe()
	}
	b.subs = nil
}
