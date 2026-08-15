// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestProxyAuditSubjects pins the subject shapes design 001 §4.5 specifies and,
// critically, that the wildcard the JetStream capture stream uses actually
// matches a concrete pane's audit subject — a mismatch there would silently
// capture nothing.
func TestProxyAuditSubjects(t *testing.T) {
	old := SessionPrefix()
	SetSessionPrefix("rysh")
	defer SetSessionPrefix(old)

	if got, want := ProxyAuditSubject("pane-1"), "rysh.pane.pane-1.proxy.audit"; got != want {
		t.Errorf("ProxyAuditSubject = %q, want %q", got, want)
	}
	if got, want := ProxyAuditWildcardSubject(), "rysh.pane.*.proxy.audit"; got != want {
		t.Errorf("ProxyAuditWildcardSubject = %q, want %q", got, want)
	}
	if got, want := ProxyAuditInboxSubject(), "rysh.proxy.audit.inbox"; got != want {
		t.Errorf("ProxyAuditInboxSubject = %q, want %q", got, want)
	}
	if !subjectMatches(ProxyAuditWildcardSubject(), ProxyAuditSubject("pane-abc")) {
		t.Errorf("wildcard %q does not match concrete %q — stream would capture nothing",
			ProxyAuditWildcardSubject(), ProxyAuditSubject("pane-abc"))
	}
}

// TestProxyAuditCodecRegistered proves the audit record round-trips through the
// DEFAULT registry — i.e. RegisterProxyAuditCodecs is actually wired into
// DefaultCodecRegistry. Without that wiring the publisher cannot even marshal
// the message (unregistered type), so the whole plane is dead.
func TestProxyAuditCodecRegistered(t *testing.T) {
	r := DefaultCodecRegistry()

	rec := &MsgProxyRequestAudit{
		PaneID: "pane-9", Dialect: "openai", Model: "gpt-x",
		Endpoint: "/v1/chat/completions", ReqBytes: 321, RedactionHits: 2,
		BudgetState: ProxyBudgetOK, Status: 200, InTokens: 11, OutTokens: 22,
		TS: time.Unix(1_700_000_000, 0).UTC(),
	}

	if tag := r.TagOf(rec); tag != TagProxyRequestAudit {
		t.Fatalf("TagOf = %q, want %q (codec not registered in DefaultCodecRegistry)", tag, TagProxyRequestAudit)
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := r.Decode(TagProxyRequestAudit, payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := decoded.(*MsgProxyRequestAudit)
	if !ok {
		t.Fatalf("decoded type = %T, want *MsgProxyRequestAudit", decoded)
	}
	if got.Dialect != "openai" || got.RedactionHits != 2 || got.BudgetState != ProxyBudgetOK || got.OutTokens != 22 {
		t.Fatalf("round-trip lost fields: %+v", got)
	}

	// The snapshot request/reply must also be registered (used by ##proxy audit).
	if tag := r.TagOf(&MsgProxyAuditSnapshotRequest{}); tag != TagProxyAuditSnapshotRequest {
		t.Fatalf("snapshot request TagOf = %q, want %q", tag, TagProxyAuditSnapshotRequest)
	}
	if tag := r.TagOf(&MsgProxyAuditSnapshotReply{}); tag != TagProxyAuditSnapshotReply {
		t.Fatalf("snapshot reply TagOf = %q, want %q", tag, TagProxyAuditSnapshotReply)
	}
}

// subjectMatches implements NATS subject matching for the token wildcard '*'.
func subjectMatches(pattern, subject string) bool {
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	if len(pt) != len(st) {
		return false
	}
	for i := range pt {
		if pt[i] == "*" {
			continue
		}
		if pt[i] != st[i] {
			return false
		}
	}
	return true
}
