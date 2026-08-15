// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// usageFakeProv is a minimal AgenticProvider: usageRecord only needs Name()
// (Model() is optional and left unimplemented, so model resolves to "").
type usageFakeProv struct{ provider.AgenticProvider }

func (usageFakeProv) Name() string { return "fake" }

// TestUsageRecord_AttributesAgent is the reachability proof for design 003
// "by agent": an orchestrator serving a named agent/humanoid must stamp that
// name onto its usage records, and a pane orchestrator must not. Without this,
// AgentName is never set and the UsageActor's ByAgent rollup is dead — exactly
// the gap the ByPane-only ledger had.
func TestUsageRecord_AttributesAgent(t *testing.T) {
	u := provider.Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 1}

	// Agent orchestrator: AgentName is set (and equals the paneID here, since an
	// agent's paneID IS its name — but the field is what the ledger keys on).
	agentOrch := &OrchestratorActor{paneID: "code-reviewer", agentName: "code-reviewer", prov: usageFakeProv{}}
	rec := agentOrch.usageRecord(u)
	if rec == nil {
		t.Fatal("usageRecord returned nil for non-empty usage")
	}
	if rec.AgentName != "code-reviewer" {
		t.Fatalf("AgentName = %q, want %q — agent spend is not attributed", rec.AgentName, "code-reviewer")
	}
	if rec.PaneID != "code-reviewer" || rec.Source != msg.UsageSourceAgent {
		t.Fatalf("unexpected record shape: %+v", rec)
	}
	if rec.InTokens != 10 || rec.OutTokens != 5 || rec.CacheRead != 1 {
		t.Fatalf("token fields wrong: %+v", rec)
	}

	// Pane orchestrator: no agent name, so AgentName stays empty (pane spend must
	// NOT leak into the by-agent view).
	paneOrch := &OrchestratorActor{paneID: "b1f9-uuid", agentName: "", prov: usageFakeProv{}}
	prec := paneOrch.usageRecord(u)
	if prec == nil || prec.AgentName != "" {
		t.Fatalf("pane record AgentName = %q, want empty", prec.AgentName)
	}

	// Empty usage produces no record (nothing to attribute).
	if got := agentOrch.usageRecord(provider.Usage{}); got != nil {
		t.Fatalf("empty usage should yield nil, got %+v", got)
	}
}
