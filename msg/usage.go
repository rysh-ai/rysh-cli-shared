// SPDX-License-Identifier: Apache-2.0

package msg

import "time"

// ---------------------------------------------------------------------------
// Usage ledger messages (roadmap design 003).
//
// One usage schema, multiple producers: rysh's own agentic orchestrator today,
// the governance proxy (design 001) and CI/eval runners (design 009) later.
// All flow to rysh.pane.{paneID}.usage where the UsageActor aggregates them.
// ---------------------------------------------------------------------------

// Usage record source discriminators. Source disambiguates producers so
// records are never double-counted (rysh's own direct calls never route
// through the proxy; the field says which producer emitted the record).
const (
	UsageSourceAgent = "agent" // rysh's own agentic orchestrator
	UsageSourceProxy = "proxy" // governance proxy (wrapped third-party CLI)
	UsageSourceEval  = "eval"  // eval harness
	UsageSourceCI    = "ci"    // headless CI run
)

// MsgUsageRecord is a single token/cost accounting record for one LLM call.
// Token counts are the provider's own reported numbers (never estimates when
// a usage block exists). CostMicroUSD is computed at record time from the
// pricing table; 0 (with a set model) means the model was not priced.
type MsgUsageRecord struct {
	PaneID    string `json:"pane_id"`
	AgentName string `json:"agent_name,omitempty"`
	// Tenant attributes this record to a customer as well as a pane
	// (design 022 §4.3). It is a SECOND INDEX over the same record, never a
	// second record: session totals are summed from the pane rollup only, so
	// tenant accounting cannot double-count `##cost`. Empty ⇒ untenanted.
	Tenant       string    `json:"tenant,omitempty"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model,omitempty"`
	Source       string    `json:"source"`
	InTokens     int       `json:"in_tokens"`
	OutTokens    int       `json:"out_tokens"`
	CacheRead    int       `json:"cache_read,omitempty"`
	CacheWrite   int       `json:"cache_write,omitempty"`
	CostMicroUSD int64     `json:"cost_micro_usd"`
	TS           time.Time `json:"ts"`
}

// MsgUsageCheck is a hot-path budget query answered by the UsageActor on the
// usage.check request/reply subject. Callers cache the reply ~2s.
type MsgUsageCheck struct {
	PaneID string `json:"pane_id"`
	// Tenant, when set, asks the ledger to check the TENANT's ceiling as well
	// as the pane's (design 022 §4.3). The stricter of the two binds: a pane
	// with headroom under a tenant that is out of budget must still be refused.
	Tenant string `json:"tenant,omitempty"`
}

// MsgUsageCheckReply reports the pane's spend against its ceiling. CeilingTokens
// == 0 means no ceiling is set (Ok is then always true).
type MsgUsageCheckReply struct {
	PaneID        string `json:"pane_id"`
	SpentTokens   int64  `json:"spent_tokens"`
	CeilingTokens int64  `json:"ceiling_tokens"`
	Ok            bool   `json:"ok"`
	// Scope names which ceiling the figures above describe: "" or "pane" for
	// the pane's own, "tenant" when the tenant's ceiling is the binding one.
	// Without it the refusal message would name the wrong budget and point the
	// operator at the wrong command.
	Scope string `json:"scope,omitempty"`
	// Tenant echoes the tenant the check was made for, when Scope == "tenant".
	Tenant string `json:"tenant,omitempty"`
}

// Usage check scopes.
const (
	UsageScopePane   = "pane"
	UsageScopeTenant = "tenant"
)

// MsgUsageSnapshotRequest asks the UsageActor for aggregates. Window is
// "today" (default) or "week".
type MsgUsageSnapshotRequest struct {
	Window string `json:"window"`
}

// UsageAgg is a rolled-up aggregate for one key (pane ID or agent name).
type UsageAgg struct {
	Key          string `json:"key"`
	Label        string `json:"label,omitempty"`
	InTokens     int64  `json:"in_tokens"`
	OutTokens    int64  `json:"out_tokens"`
	CacheRead    int64  `json:"cache_read"`
	CacheWrite   int64  `json:"cache_write"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Calls        int64  `json:"calls"`
	// UnknownCost is set when at least one record folded into this aggregate
	// had a model with no pricing entry — cost is then a lower bound and the
	// UI flags it with a tilde rather than inventing a dollar figure.
	UnknownCost bool `json:"unknown_cost,omitempty"`
}

// UsageCeiling reports a pane's configured token ceiling and current spend.
type UsageCeiling struct {
	PaneID        string `json:"pane_id"`
	CeilingTokens int64  `json:"ceiling_tokens"`
	SpentTokens   int64  `json:"spent_tokens"`
}

// MsgUsageSnapshotReply carries the aggregates for a window.
type MsgUsageSnapshotReply struct {
	Window              string         `json:"window"`
	SessionCostMicroUSD int64          `json:"session_cost_micro_usd"`
	SessionTokens       int64          `json:"session_tokens"`
	ByPane              []UsageAgg     `json:"by_pane"`
	ByAgent             []UsageAgg     `json:"by_agent"`
	Ceilings            []UsageCeiling `json:"ceilings,omitempty"`
}

// MsgUsageBudgetSet sets (or clears, with CeilingTokens == 0) a pane's soft
// alarm + hard token ceiling used by the usage.check endpoint.
type MsgUsageBudgetSet struct {
	PaneID        string `json:"pane_id"`
	CeilingTokens int64  `json:"ceiling_tokens"`
}

// TypeTags for usage messages.
const (
	TagUsageRecord          = "MsgUsageRecord"
	TagUsageCheck           = "MsgUsageCheck"
	TagUsageCheckReply      = "MsgUsageCheckReply"
	TagUsageSnapshotRequest = "MsgUsageSnapshotRequest"
	TagUsageSnapshotReply   = "MsgUsageSnapshotReply"
	TagUsageBudgetSet       = "MsgUsageBudgetSet"
)

// UsageSubject is the per-pane usage record subject.
func UsageSubject(paneID string) string { return T("pane", paneID, "usage") }

// UsageWildcardSubject matches every pane's usage subject (UsageActor sub).
func UsageWildcardSubject() string { return T("pane", "*", "usage") }

// UsageCheckSubject is the request/reply budget-check subject.
func UsageCheckSubject() string { return T("usage", "check") }

// UsageInboxSubject is the UsageActor control/snapshot inbox subject.
func UsageInboxSubject() string { return T("usage", "inbox") }

// SendUsageRecord publishes a usage record to the producing pane's usage
// subject (data-plane publish — no mailbox hop).
func (p *NATSPublisher) SendUsageRecord(rec *MsgUsageRecord) error {
	return p.Send(UsageSubject(rec.PaneID), rec)
}

// RegisterUsageCodecs registers all usage message codecs on r.
func RegisterUsageCodecs(r *CodecRegistry) {
	r.Register(TagUsageRecord, "*msg.MsgUsageRecord", jsonDecoder[MsgUsageRecord]())
	r.Register(TagUsageCheck, "*msg.MsgUsageCheck", jsonDecoder[MsgUsageCheck]())
	r.Register(TagUsageCheckReply, "*msg.MsgUsageCheckReply", jsonDecoder[MsgUsageCheckReply]())
	r.Register(TagUsageSnapshotRequest, "*msg.MsgUsageSnapshotRequest", jsonDecoder[MsgUsageSnapshotRequest]())
	r.Register(TagUsageSnapshotReply, "*msg.MsgUsageSnapshotReply", jsonDecoder[MsgUsageSnapshotReply]())
	r.Register(TagUsageBudgetSet, "*msg.MsgUsageBudgetSet", jsonDecoder[MsgUsageBudgetSet]())
}
