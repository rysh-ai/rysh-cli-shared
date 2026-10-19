// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	store          Store
	now            time.Time
	leader, worker Request
	la, wa         Auth
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: NewStore(), now: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	f.la = Auth{UserID: uuid.NewString(), WorkspaceRole: "owner", PeerToken: strings.Repeat("a", 64)}
	f.wa = Auth{UserID: uuid.NewString(), WorkspaceRole: "member", PeerToken: strings.Repeat("b", 64)}
	a := f.call(t, f.la, Request{Op: "init", SessionID: uuid.NewString(), SessionName: "dev", Incarnation: uuid.NewString(), Name: "leader", Runner: "claude"})
	f.leader = Request{PeerID: a.Peer.ID, AgentID: a.Agent.ID, Incarnation: a.Peer.Incarnation, Term: 1}
	b := f.call(t, f.wa, Request{Op: "join", SessionID: uuid.NewString(), SessionName: "remote", Incarnation: uuid.NewString(), Name: "backend", Role: "worker", Runner: "codex", Projects: []string{"api"}, MaxActive: 1})
	f.worker = Request{PeerID: b.Peer.ID, AgentID: b.Agent.ID, Incarnation: b.Peer.Incarnation}
	for _, item := range []struct {
		a Auth
		r Request
	}{{f.la, f.leader}, {f.wa, f.worker}} {
		r := item.r
		r.Op = "heartbeat"
		r.Ready = true
		r.ActiveAgents = []string{r.AgentID}
		f.call(t, item.a, r)
	}
	return f
}
func (f *fixture) call(t *testing.T, a Auth, r Request) Response {
	t.Helper()
	r.ID = uuid.NewString()
	r.SchemaVersion = Version
	s, out, _, err := Apply(f.store, a, r, f.now)
	if err != nil {
		t.Fatal(r.Op, err)
	}
	f.store = s
	return out
}
func (f *fixture) reject(t *testing.T, a Auth, r Request, code string) {
	t.Helper()
	r.ID = uuid.NewString()
	r.SchemaVersion = Version
	before, _ := json.Marshal(f.store)
	s, _, _, err := Apply(f.store, a, r, f.now)
	if err == nil {
		t.Fatalf("%s unexpectedly accepted", r.Op)
	}
	if e, ok := err.(*Error); !ok || e.Code != code {
		t.Fatalf("%s error = %v, want %s", r.Op, err, code)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("failed transition mutated original state")
	}
}
func (f *fixture) assign(t *testing.T, deps ...string) *Task {
	r := f.leader
	r.Op = "task.assign"
	r.Target = f.worker.AgentID
	r.Project = "api"
	r.Runner = "codex"
	r.Text = "Implement a small endpoint"
	r.Criteria = "Endpoint test passes"
	r.MaxRuntimeSeconds = 60
	r.Dependencies = deps
	return f.call(t, f.la, r).Task
}
func (f *fixture) claim(t *testing.T, task *Task) (Request, Auth) {
	r := f.worker
	r.Op = "task.claim"
	r.TaskID = task.ID
	r.AttemptID = task.Current().ID
	r.TaskVersion = task.Version
	r.ExecutionID = uuid.NewString()
	a := f.wa
	a.ExecutionToken = strings.Repeat("x", 64)
	f.call(t, a, r)
	return r, a
}
func TestCoordinationRegistrationIdentityAndAuthority(t *testing.T) {
	f := newFixture(t)
	r := f.leader
	r.Op = "task.assign"
	r.Target = f.worker.AgentID
	r.Project = "api"
	r.Runner = "codex"
	r.Text = "work"
	r.Criteria = "test"
	r.MaxRuntimeSeconds = 60
	f.reject(t, f.wa, r, "forbidden")
	r = f.worker
	r.Op = "leader.handover"
	r.Target = f.worker.AgentID
	f.reject(t, f.wa, r, "stale")
	r = Request{Op: "join", SessionID: f.store.State.Peers[f.leader.PeerID].SessionID, SessionName: "dev", Incarnation: uuid.NewString()}
	bad := f.la
	bad.PeerToken = strings.Repeat("c", 64)
	f.reject(t, bad, r, "conflict")
	other := f.call(t, f.wa, Request{Op: "join", SessionID: uuid.NewString(), SessionName: "dev", Incarnation: uuid.NewString(), Name: "leader", Role: "leader", Runner: "claude", MaxActive: 1})
	if _, err := f.store.State.ResolveAgent("dev/leader"); err == nil {
		t.Fatal("ambiguous label resolved")
	}
	if f.store.State.Leadership.AgentID == other.Agent.ID {
		t.Fatal("joining leader candidate seized leadership")
	}
	data, _ := json.Marshal(f.store.State)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), HashToken(f.la.PeerToken)) {
		t.Fatal("public state leaked credential hashes")
	}
}
func TestTaskClaimResultReviewAndDependencies(t *testing.T) {
	f := newFixture(t)
	first := f.assign(t)
	second := f.assign(t, first.ID)
	blocked := f.worker
	blocked.Op = "task.claim"
	blocked.TaskID = second.ID
	blocked.AttemptID = second.Current().ID
	blocked.TaskVersion = second.Version
	blocked.ExecutionID = uuid.NewString()
	auth := f.wa
	auth.ExecutionToken = strings.Repeat("z", 64)
	f.reject(t, auth, blocked, "unavailable")
	r, a := f.claim(t, first)
	f.reject(t, a, r, "conflict")
	r.Op = "task.start"
	r.PaneID = uuid.NewString()
	started := f.call(t, a, r).Task
	if started.State != "running" {
		t.Fatal(started.State)
	}
	r.Op = "task.result"
	r.Result = json.RawMessage(`{"summary":"Endpoint implemented","tests":["go test ./..."]}`)
	result := f.call(t, a, r).Task
	if result.State != "result_submitted" {
		t.Fatal(result.State)
	}
	review := f.leader
	review.Op = "task.review"
	review.TaskID = first.ID
	review.TaskVersion = result.Version
	review.Ready = true
	f.call(t, f.la, review)
	f.claim(t, f.store.State.Tasks[second.ID])
}
func TestLeadershipFencingAndExplicitHandover(t *testing.T) {
	f := newFixture(t)
	task := f.assign(t)
	candidate := f.call(t, f.wa, Request{Op: "join", SessionID: uuid.NewString(), SessionName: "backup", Incarnation: uuid.NewString(), Name: "leader", Role: "leader", Runner: "claude", MaxActive: 1})
	handover := f.leader
	handover.Op = "leader.handover"
	handover.Target = candidate.Agent.ID
	f.call(t, f.la, handover)
	if f.store.State.Leadership.Term != 2 || f.store.State.Tasks[task.ID].LeaderTerm != 2 {
		t.Fatal("pending offer did not transfer to new term")
	}
	stale := f.leader
	stale.Op = "task.assign"
	f.reject(t, f.la, stale, "stale")
	f.claim(t, f.store.State.Tasks[task.ID])
}
func TestRestartRequiresAdoptionAndExpiredClaimsDoNotStart(t *testing.T) {
	f := newFixture(t)
	task := f.assign(t)
	r, a := f.claim(t, task)
	f.now = f.now.Add(31 * time.Second)
	r.Op = "task.start"
	r.PaneID = uuid.NewString()
	f.reject(t, a, r, "conflict")
	p := f.store.State.Peers[f.worker.PeerID]
	joined := f.call(t, f.wa, Request{Op: "join", SessionID: p.SessionID, SessionName: p.SessionName, Incarnation: uuid.NewString()})
	r.Op = "task.adopt"
	r.Incarnation = joined.Peer.Incarnation
	adopted := f.call(t, a, r).Task
	if adopted.Current().ExecutionID != r.ExecutionID || len(adopted.Attempts) != 1 {
		t.Fatal("adoption created another execution")
	}
	r.Op = "task.start"
	f.call(t, a, r)
	old := f.worker
	old.Op = "heartbeat"
	f.reject(t, f.wa, old, "stale")
}
func TestCapacityCancellationAndReconciliation(t *testing.T) {
	f := newFixture(t)
	first := f.assign(t)
	second := f.assign(t)
	r, a := f.claim(t, first)
	other := f.worker
	other.Op = "task.claim"
	other.TaskID = second.ID
	other.AttemptID = second.Current().ID
	other.TaskVersion = second.Version
	other.ExecutionID = uuid.NewString()
	f.reject(t, a, other, "capacity")
	cancel := f.leader
	cancel.Op = "task.cancel"
	cancel.TaskID = first.ID
	f.call(t, f.la, cancel)
	r.Op = "task.result"
	r.Result = json.RawMessage(`{"summary":"late success"}`)
	f.reject(t, a, r, "stale")
	retry := f.leader
	retry.Op = "task.retry"
	retry.TaskID = first.ID
	retry.Target = f.worker.AgentID
	f.reject(t, f.la, retry, "conflict")
	r.Op = "task.cancelled"
	f.call(t, a, r)
	retried := f.call(t, f.la, retry).Task
	if len(retried.Attempts) != 2 || retried.Current().ExecutionID != "" {
		t.Fatal("retry did not create a fresh, unlaunched attempt")
	}
}
func TestStandbyElectionRequiresApprovedReadyOrder(t *testing.T) {
	f := newFixture(t)
	candidate := f.call(t, f.wa, Request{Op: "join", SessionID: uuid.NewString(), SessionName: "backup", Incarnation: uuid.NewString(), Name: "leader", Role: "leader", Runner: "claude", MaxActive: 1})
	r := Request{PeerID: candidate.Peer.ID, AgentID: candidate.Agent.ID, Incarnation: candidate.Peer.Incarnation, Op: "leader.acquire"}
	f.reject(t, f.wa, r, "conflict")
	policy := f.leader
	policy.Op = "leader.standbys"
	policy.AutoFailover = true
	policy.Standbys = []string{candidate.Agent.ID}
	f.call(t, f.la, policy)
	f.now = f.now.Add(31 * time.Second)
	hb := r
	hb.Op = "heartbeat"
	hb.Ready = true
	hb.ActiveAgents = []string{candidate.Agent.ID}
	f.call(t, f.wa, hb)
	f.call(t, f.wa, r)
	if f.store.State.Leadership.AgentID != candidate.Agent.ID || f.store.State.Leadership.Term != 2 {
		t.Fatal("standby did not acquire a new term")
	}
}
