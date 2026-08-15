// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// ApprovalManager handles sending approval requests and receiving responses.
// It is used by the TUI to publish user decisions back to the orchestrator.
//
// The TUI calls PublishApproval when the user makes a decision on a pending
// approval request (displayed as a diff or confirmation prompt).
type ApprovalManager struct {
	nc     *nats.Conn
	pub    *msg.NATSPublisher
	paneID string
}

// NewApprovalManager creates an ApprovalManager for a specific pane.
func NewApprovalManager(nc *nats.Conn, pub *msg.NATSPublisher, paneID string) *ApprovalManager {
	return &ApprovalManager{
		nc:     nc,
		pub:    pub,
		paneID: paneID,
	}
}

// PublishApproval publishes an approval response to the orchestrator.
func (am *ApprovalManager) PublishApproval(requestID string, decision msg.ApprovalDecision, reason string) {
	subject := msg.T("pane", am.paneID, "approval", "response")
	resp := &msg.MsgApprovalResponse{
		RequestID: requestID,
		Decision:  decision,
		Reason:    reason,
	}
	if err := am.pub.Send(subject, resp); err != nil {
		slog.Error("approval: publish response", "err", err, "pane", am.paneID)
	}
}

// SubscribeApprovalRequests subscribes to approval requests for this pane.
// The callback is invoked for each request, allowing the TUI to render the
// approval prompt.
func (am *ApprovalManager) SubscribeApprovalRequests(callback func(*msg.MsgApprovalRequest)) (*nats.Subscription, error) {
	subject := msg.T("pane", am.paneID, "approval", "request")
	return am.nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		if env.TypeTag != msg.TagApprovalRequest {
			return
		}
		var req msg.MsgApprovalRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return
		}
		callback(&req)
	})
}

// ApprovalRequestSubject returns the NATS subject for approval requests.
func ApprovalRequestSubject(paneID string) string {
	return msg.T("pane", paneID, "approval", "request")
}

// ApprovalResponseSubject returns the NATS subject for approval responses.
func ApprovalResponseSubject(paneID string) string {
	return msg.T("pane", paneID, "approval", "response")
}
