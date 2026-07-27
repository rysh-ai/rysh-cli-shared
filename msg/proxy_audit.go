package msg

import "time"

// ---------------------------------------------------------------------------
// Governance-proxy audit plane (roadmap design 001 §4.5).
//
// Every request the governance proxy forwards emits one MsgProxyRequestAudit on
// rysh.pane.{paneID}.proxy.audit (a data-plane publish, like the usage record).
// This is the DURABLE record of governed traffic: the proxy also keeps a small
// in-process ring for the current process, but that ring dies with the daemon.
// The audit subject is captured by a JetStream stream (ProxyAuditActor) so the
// trail survives a restart, and design 013's org-audit forwarding subscribes to
// this same subject — one schema, two consumers.
//
// The record is METADATA ONLY. It never carries request or response content;
// the optional `audit_content` body store (off by default) is a separate,
// proxy-side, redacted-only file sink and is not part of this message.
// ---------------------------------------------------------------------------

// Budget states for MsgProxyRequestAudit.BudgetState.
const (
	// ProxyBudgetOK — the request was within the pane's token ceiling (or no
	// ceiling was set) and was forwarded upstream.
	ProxyBudgetOK = "ok"
	// ProxyBudgetExceeded — the request was refused at the proxy because the
	// pane was over its ceiling; no upstream call was made.
	ProxyBudgetExceeded = "exceeded"
)

// MsgProxyRequestAudit is a metadata-only record of one proxied request
// (design 001 §4.5). Token counts are the provider's own reported numbers when
// present. It NEVER carries request/response bodies.
type MsgProxyRequestAudit struct {
	PaneID        string    `json:"pane_id"`
	Dialect       string    `json:"dialect"`             // anthropic | openai | gemini
	Model         string    `json:"model,omitempty"`     // provider-reported model, if known
	Endpoint      string    `json:"endpoint"`            // upstream path, or "(budget)" for a refusal
	ReqBytes      int       `json:"req_bytes"`           // size of the (redacted) request body
	RedactionHits int       `json:"redaction_hits"`      // secrets SNAT redacted before forwarding
	BudgetState   string    `json:"budget_state"`        // ok | exceeded
	Status        int       `json:"status"`              // upstream HTTP status, or 429 for a refusal
	InTokens      int       `json:"in_tokens,omitempty"` // provider-reported input tokens
	OutTokens     int       `json:"out_tokens,omitempty"`
	TS            time.Time `json:"ts"`
}

// MsgProxyAuditSnapshotRequest asks the ProxyAuditActor for the most recent
// audit records (request/reply on the proxy-audit inbox). Limit <= 0 uses the
// actor's default.
type MsgProxyAuditSnapshotRequest struct {
	Limit int `json:"limit"`
}

// MsgProxyAuditSnapshotReply carries recent audit records, oldest first, for
// the ##proxy audit view. Durable reports whether the records came from the
// JetStream-backed trail (survives restart) or only the current process.
type MsgProxyAuditSnapshotReply struct {
	Records []MsgProxyRequestAudit `json:"records"`
	Durable bool                   `json:"durable"`
}

// TypeTags for the proxy-audit messages.
const (
	TagProxyRequestAudit         = "MsgProxyRequestAudit"
	TagProxyAuditSnapshotRequest = "MsgProxyAuditSnapshotRequest"
	TagProxyAuditSnapshotReply   = "MsgProxyAuditSnapshotReply"
)

// ProxyAuditSubject is the per-pane proxy audit subject (data-plane publish).
func ProxyAuditSubject(paneID string) string { return T("pane", paneID, "proxy", "audit") }

// ProxyAuditWildcardSubject matches every pane's proxy audit subject. It is the
// JetStream stream's capture filter.
func ProxyAuditWildcardSubject() string { return T("pane", "*", "proxy", "audit") }

// ProxyAuditInboxSubject is the ProxyAuditActor's control/snapshot inbox
// (request/reply for ##proxy audit).
func ProxyAuditInboxSubject() string { return T("proxy", "audit", "inbox") }

// SendProxyAudit publishes a proxy audit record to the producing pane's proxy
// audit subject (data-plane publish — no mailbox hop).
func (p *NATSPublisher) SendProxyAudit(rec *MsgProxyRequestAudit) error {
	return p.Send(ProxyAuditSubject(rec.PaneID), rec)
}

// RegisterProxyAuditCodecs registers the proxy-audit codecs on r.
func RegisterProxyAuditCodecs(r *CodecRegistry) {
	r.Register(TagProxyRequestAudit, "*msg.MsgProxyRequestAudit", jsonDecoder[MsgProxyRequestAudit]())
	r.Register(TagProxyAuditSnapshotRequest, "*msg.MsgProxyAuditSnapshotRequest", jsonDecoder[MsgProxyAuditSnapshotRequest]())
	r.Register(TagProxyAuditSnapshotReply, "*msg.MsgProxyAuditSnapshotReply", jsonDecoder[MsgProxyAuditSnapshotReply]())
}
