// SPDX-License-Identifier: Apache-2.0

package msg

import "time"

// ---------------------------------------------------------------------------
// LLM gateway control plane (roadmap design 023).
//
// 022 gave the governance proxy failover, request-rate limits, per-tenant
// policy and ungoverned-CLI detection — all enforced INSIDE one daemon, against
// a per-session KV ledger. So a tenant with a 2M-token ceiling gets 2M PER
// LAPTOP. This is the wire contract that makes a ceiling hold across machines.
//
// THE SHAPE, AND WHY
// ------------------
// The daemon NEVER asks the server whether a request is allowed (023 §4.2/G4).
// A per-request WAN call would put provider-latency-scale delay in front of
// every request and make a network blip look like a provider outage. Instead:
//
//	usage   → fire-and-forget batches over the already-connected NATS link
//	leases  → a synchronous HTTP call that hands the daemon a SLICE of the
//	          allowance, which it then enforces locally and in-process
//	policy  → an HTTP GET with an ETag, polled
//
// Everything here is METADATA ONLY: pane ids, tenant names, token counts, cost.
// No prompts, no completions, no content — the same posture as the proxy audit
// plane (001 §4.5) and 013 §3's audit forwarding.
//
// None of it activates unless BOTH `upstream.enabled` and the explicit
// `upstream.governance` opt-in are set. Reporting local spend to a server is a
// data-egress decision, and the OSS client must stay fully functional
// standalone (023 §1).
// ---------------------------------------------------------------------------

// GatewayUsageRollup is one (tenant, day) aggregate of governed spend.
//
// The daemon rolls its records up before sending rather than shipping one
// message per request: the server table is keyed (workspace, tenant, day), so a
// per-request stream would be N inserts collapsing to the same row, and a busy
// agent makes a lot of requests.
type GatewayUsageRollup struct {
	// Tenant is the customer dimension from 022 §4.3. "" = untenanted.
	Tenant string `json:"tenant,omitempty"`
	// UsageDate is the UTC calendar day, "YYYY-MM-DD" — the same string form
	// invoke_op_usage uses, so the unique key is a stable upsert target.
	UsageDate    string `json:"usage_date"`
	InTokens     int64  `json:"in_tokens"`
	OutTokens    int64  `json:"out_tokens"`
	CacheRead    int64  `json:"cache_read,omitempty"`
	CacheWrite   int64  `json:"cache_write,omitempty"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Calls        int64  `json:"calls"`
}

// Tokens is what a ceiling is denominated in (023 §4.2: leases are TOKENS, not
// cost — every ceiling in rysh is already written in tokens, and a second unit
// on the hot path would need a translation nobody wants there).
func (r GatewayUsageRollup) Tokens() int64 { return r.InTokens + r.OutTokens }

// MsgGatewayUsageBatch is one flush of governed spend to the server
// (design 023 §4.4), published on ws.{workspaceID}.gateway.usage.
//
// IDEMPOTENCY IS THE POINT OF (DaemonID, Seq). NATS redelivery and daemon
// reconnects both replay batches; without a seen-set the aggregate silently
// inflates on every reconnect — the same class of bug 022 §4.3 avoided by
// refusing to write a second usage record per request.
type MsgGatewayUsageBatch struct {
	// DaemonID identifies the sending daemon for the lifetime of its process.
	DaemonID string `json:"daemon_id"`
	// Seq increments per batch within one daemon, starting at 1.
	Seq int64 `json:"seq"`
	// WorkspaceID is carried in the payload as well as the subject so a
	// consumer that fans in from several subjects does not have to parse it
	// back out of the subject token.
	WorkspaceID string               `json:"workspace_id"`
	Rollups     []GatewayUsageRollup `json:"rollups"`
	SentAt      time.Time            `json:"sent_at"`
}

// TagGatewayUsageBatch is the codec tag for MsgGatewayUsageBatch.
const TagGatewayUsageBatch = "MsgGatewayUsageBatch"

// GatewayUsageSubject is where a daemon publishes its usage batches. It sits
// inside the workspace namespace the daemon's API key is already scoped to
// (ws.{id}.>), so the existing subject ACL authorises it with no new rule.
func GatewayUsageSubject(workspaceID string) string {
	return T("ws", workspaceID, "gateway", "usage")
}

// GatewayUsageWildcardSubject matches every workspace's batches — the server's
// single fan-in subscription.
func GatewayUsageWildcardSubject() string { return T("ws", "*", "gateway", "usage") }

// SendGatewayUsage publishes a usage batch (data-plane publish, no mailbox hop).
func (p *NATSPublisher) SendGatewayUsage(b *MsgGatewayUsageBatch) error {
	return p.Send(GatewayUsageSubject(b.WorkspaceID), b)
}

// RegisterGatewayCodecs registers the gateway control-plane codecs on r.
func RegisterGatewayCodecs(r *CodecRegistry) {
	r.Register(TagGatewayUsageBatch, "*msg.MsgGatewayUsageBatch", jsonDecoder[MsgGatewayUsageBatch]())
}

// ---------------------------------------------------------------------------
// Lease (HTTP): POST /api/workspaces/{wsID}/gateway/lease
// ---------------------------------------------------------------------------

// GatewayLeaseRequest asks for a slice of a scope's remaining allowance.
//
// SpentSince piggybacks the daemon's spend since its last lease call, so the
// server reconciles on a round trip the renewal already costs (023 §4.2).
type GatewayLeaseRequest struct {
	// Scope is "" for the whole workspace, or "tenant:<name>" for one customer.
	Scope      string `json:"scope,omitempty"`
	WantTokens int64  `json:"want_tokens"`
	DaemonID   string `json:"daemon_id"`
	SpentSince int64  `json:"spent_since"`
	// SpentSinceMicroUSD is the same piggyback in money, for the spend cap
	// (root-order-044 #8). The server takes the larger of it and its own
	// rollup, exactly as it does for tokens. Omitted when zero, so an old
	// server sees the request it always saw.
	SpentSinceMicroUSD int64 `json:"spent_since_micro_usd,omitempty"`
}

// GatewayLeaseReply is the grant.
//
// GrantedTokens == 0 means the scope is out of budget: the daemon refuses
// locally with the same dialect-shaped 429 the machine-local ceiling produces,
// so a wrapped CLI cannot tell the two apart (023 §4.2).
type GatewayLeaseReply struct {
	GrantedTokens  int64     `json:"granted_tokens"`
	ExpiresAt      time.Time `json:"expires_at"`
	RemainingToday int64     `json:"remaining_today"`
	// CeilingTokens is the scope's configured ceiling, 0 when unlimited. With
	// no ceiling the server grants freely and the daemon still reports usage.
	CeilingTokens int64 `json:"ceiling_tokens"`
	// ActiveDaemons and MaxLeaseTokens make the OVERSPEND BOUND observable
	// rather than a claim in a document: worst case is
	// ceiling + ActiveDaemons × MaxLeaseTokens (023 §4.2).
	ActiveDaemons  int   `json:"active_daemons"`
	MaxLeaseTokens int64 `json:"max_lease_tokens"`

	// Refusal names which limit bound when GrantedTokens is 0: "" (none, or an
	// old server), GatewayRefusalTokenCeiling or GatewayRefusalSpendCap. The
	// daemon refuses on GrantedTokens == 0 either way; Refusal only changes the
	// words, so an old client still fails closed on a spend-capped workspace.
	Refusal string `json:"refusal,omitempty"`
	// The spend cap (root-order-044 #8), present only when the workspace has
	// one. The figures are rysh's ESTIMATE from its price table, not a
	// provider invoice. The cap can be overshot by the leases already out:
	// ActiveDaemons × MaxLeaseTokens at the dearest model's price.
	SpendCapMicroUSD    int64  `json:"spend_cap_micro_usd,omitempty"`
	SpentPeriodMicroUSD int64  `json:"spent_period_micro_usd,omitempty"`
	SpendCapPeriod      string `json:"spend_cap_period,omitempty"`
}

// Lease refusal reasons (GatewayLeaseReply.Refusal).
const (
	GatewayRefusalTokenCeiling = "token_ceiling"
	GatewayRefusalSpendCap     = "spend_cap"
)

// ---------------------------------------------------------------------------
// Spend cap (HTTP): GET/PUT/DELETE /api/workspaces/{wsID}/gateway/spend-cap
// ---------------------------------------------------------------------------

// Spend cap periods. UTC calendar boundaries only: a cap that reset at each
// viewer's local midnight would have no single answer to "how much this month".
const (
	SpendCapPeriodDay   = "day"
	SpendCapPeriodMonth = "month"
)

// ValidSpendCapPeriod reports whether p is a period the server accepts. Exact
// match only: "Month" or " month" are refused, not normalised.
func ValidSpendCapPeriod(p string) bool {
	return p == SpendCapPeriodDay || p == SpendCapPeriodMonth
}

// SpendCapPeriodBounds returns the UTC period [start, end) containing now.
// Exported because the server's sum and the client's wording must agree on
// when a period starts. ok is false for an unknown period.
func SpendCapPeriodBounds(period string, now time.Time) (start, end time.Time, ok bool) {
	u := now.UTC()
	switch period {
	case SpendCapPeriodDay:
		start = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1), true
	case SpendCapPeriodMonth:
		start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), true
	}
	return time.Time{}, time.Time{}, false
}

// GatewaySpendCap is the spend-cap document. PUT takes Period and CapMicroUSD;
// GET also fills SpentMicroUSD (the estimate for the current period) and the
// period bounds.
type GatewaySpendCap struct {
	Period        string    `json:"period"`
	CapMicroUSD   int64     `json:"cap_micro_usd"`
	SpentMicroUSD int64     `json:"spent_micro_usd,omitempty"`
	PeriodStart   time.Time `json:"period_start,omitzero"`
	PeriodEnd     time.Time `json:"period_end,omitzero"`
}

// GatewayLeaseTTL is how long a grant is valid. Short on purpose: a dead
// daemon's slice returns to the pool within one TTL (023 §7).
const GatewayLeaseTTL = 5 * time.Minute

// Lease sizing defaults (023 §4.2). Exported because the client's renewal
// arithmetic and the server's grant arithmetic must agree, and duplicating
// numbers across a network boundary is how they stop agreeing.
const (
	// GatewayLeaseWantFraction is the share of the remaining allowance a
	// daemon asks for.
	GatewayLeaseWantFraction = 0.10
	// GatewayLeaseMinTokens floors the ask, so a nearly-exhausted ceiling does
	// not degrade into a lease request per request.
	GatewayLeaseMinTokens int64 = 50_000
	// GatewayLeaseMaxTokens ceilings one grant — and therefore, multiplied by
	// the number of daemons, the whole model's worst-case overspend.
	GatewayLeaseMaxTokens int64 = 1_000_000
	// GatewayLeaseRenewFraction triggers renewal once this much of the lease
	// is left...
	GatewayLeaseRenewFraction = 0.20
	// ...or this long before expiry, whichever comes first.
	GatewayLeaseRenewBefore = 60 * time.Second
)

// TenantScope renders a tenant as a lease/policy scope. Untenanted traffic maps
// to the workspace scope ("") rather than to a tenant named "".
func TenantScope(tenant string) string {
	if tenant == "" {
		return ""
	}
	return "tenant:" + tenant
}
