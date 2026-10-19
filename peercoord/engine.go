// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidName(s string) bool { return namePattern.MatchString(s) }
func ValidID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func HashToken(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func tokenMatches(s, h string) bool {
	return len(s) >= 32 && len(h) == 64 && subtle.ConstantTimeCompare([]byte(HashToken(s)), []byte(h)) == 1
}
func validLabel(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 128 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func validText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= MaxText && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func isAdmin(a Auth) bool { return a.WorkspaceRole == "owner" || a.WorkspaceRole == "admin" }

// Apply is a deterministic transition over a copy. Failed requests cannot mutate
// the original, and persistence/commit ordering belongs to the server transaction.
func Apply(original Store, a Auth, r Request, now time.Time) (Store, Response, []Event, error) {
	response := Response{SchemaVersion: Version, ServerTime: now}
	if r.SchemaVersion != Version {
		return original, response, nil, Fail("version", "coordination protocol version %d required", Version)
	}
	if !ValidID(r.ID) {
		return original, response, nil, Fail("invalid", "request id must be a canonical UUID")
	}
	if a.UserID == "" || a.WorkspaceRole == "" || a.WorkspaceRole == "observer" {
		return original, response, nil, Fail("forbidden", "workspace write membership required")
	}
	copy := cloneStore(original)
	if copy.State.Peers == nil {
		copy = NewStore()
	}
	reducer := transition{s: &copy, a: a, r: r, now: now, response: response}
	if e := reducer.apply(); e != nil {
		return original, response, nil, e
	}
	copy.State.ServerTime = now
	return copy, reducer.response, reducer.events, nil
}

type transition struct {
	s        *Store
	a        Auth
	r        Request
	now      time.Time
	response Response
	events   []Event
}

func (x *transition) event(kind, to, task, thread, text string) {
	id := x.r.ID
	if len(x.events) > 0 {
		id = uuid.NewSHA1(uuid.MustParse(x.r.ID), []byte(fmt.Sprint(len(x.events)))).String()
	}
	if thread == "" {
		thread = id
	}
	event := Event{SchemaVersion: Version, ID: id, Kind: kind, SenderID: x.r.AgentID, RecipientID: to, TaskID: task, ThreadID: thread, Text: text, CreatedAt: x.now}
	if t := x.s.State.Tasks[task]; t != nil && t.Current() != nil {
		event.AttemptID = t.Current().ID
	}
	x.events = append(x.events, event)
	x.response.Event = &x.events[len(x.events)-1]
}
func (x *transition) peer() (*Peer, error) {
	p := x.s.State.Peers[x.r.PeerID]
	if p == nil || p.UserID != x.a.UserID || !tokenMatches(x.a.PeerToken, x.s.PeerSecrets[p.ID]) || p.State == "revoked" {
		return nil, Fail("forbidden", "peer registration credential refused")
	}
	if x.r.Incarnation != p.Incarnation {
		return nil, Fail("stale", "another daemon incarnation owns this registration; rejoin explicitly")
	}
	return p, nil
}
func (x *transition) ownAgent() (*Agent, error) {
	p, e := x.peer()
	if e != nil {
		return nil, e
	}
	a := x.s.State.Agents[x.r.AgentID]
	if a == nil || a.PeerID != p.ID {
		return nil, Fail("forbidden", "agent is not owned by this peer")
	}
	return a, nil
}
func (x *transition) leader() (*Agent, error) {
	a, e := x.ownAgent()
	if e != nil {
		return nil, e
	}
	l := x.s.State.Leadership
	if a.ID != l.AgentID || x.r.Term != l.Term || !x.now.Before(l.LeaseUntil) || x.s.State.Paused {
		return nil, Fail("stale", "current active leader term required")
	}
	return a, nil
}
func (x *transition) join() (*Peer, error) {
	if len(x.a.PeerToken) < 32 || len(x.a.PeerToken) > 256 || !ValidID(x.r.SessionID) || !ValidID(x.r.Incarnation) || !validLabel(x.r.SessionName) {
		return nil, Fail("invalid", "session UUID, incarnation, label and registration credential required")
	}
	for _, p := range x.s.State.Peers {
		if p.UserID == x.a.UserID && p.SessionID == x.r.SessionID {
			if p.State == "revoked" || !tokenMatches(x.a.PeerToken, x.s.PeerSecrets[p.ID]) {
				return nil, Fail("conflict", "session already registered with another credential; operator reset required")
			}
			p.Incarnation = x.r.Incarnation
			p.SessionName = x.r.SessionName
			p.State = "registered"
			p.LastSeen = x.now
			p.LeaseUntil = x.now.Add(LeaseDuration)
			for _, a := range x.s.State.Agents {
				if a.PeerID == p.ID {
					a.Label = p.SessionName + "/" + a.Name
					a.Ready = false
				}
			}
			return p, nil
		}
	}
	if len(x.s.State.Peers) >= 100 {
		return nil, Fail("capacity", "board registration limit reached")
	}
	p := &Peer{ID: uuid.NewString(), UserID: x.a.UserID, SessionID: x.r.SessionID, SessionName: x.r.SessionName, Incarnation: x.r.Incarnation, State: "registered", LastSeen: x.now, LeaseUntil: x.now.Add(LeaseDuration)}
	x.s.State.Peers[p.ID] = p
	x.s.PeerSecrets[p.ID] = HashToken(x.a.PeerToken)
	return p, nil
}
func (x *transition) register(p *Peer) (*Agent, error) {
	if !ValidName(x.r.Name) || (x.r.Role != "worker" && x.r.Role != "leader") || (x.r.Runner != "claude" && x.r.Runner != "codex") || x.r.MaxActive < 1 || x.r.MaxActive > 16 || len(x.r.Projects) > 32 || len(x.r.Capabilities) > 32 {
		return nil, Fail("invalid", "valid name, worker/leader role, claude/codex runner and capacity 1..16 required")
	}
	for _, s := range append(append([]string{}, x.r.Projects...), x.r.Capabilities...) {
		if !ValidName(s) {
			return nil, Fail("invalid", "invalid project or capability name")
		}
	}
	for _, a := range x.s.State.Agents {
		if a.PeerID == p.ID && a.Name == x.r.Name {
			if a.Role != x.r.Role || a.Runner != x.r.Runner || !slices.Equal(a.Projects, x.r.Projects) || !slices.Equal(a.Capabilities, x.r.Capabilities) || a.MaxActive != x.r.MaxActive {
				return nil, Fail("conflict", "endpoint already registered with another profile; leave it before replacing its policy")
			}
			return a, nil
		}
	}
	if len(x.s.State.Agents) >= MaxAgents {
		return nil, Fail("capacity", "board agent limit reached")
	}
	a := &Agent{ID: uuid.NewString(), PeerID: p.ID, Name: x.r.Name, Label: p.SessionName + "/" + x.r.Name, Role: x.r.Role, Runner: x.r.Runner, Projects: x.r.Projects, Capabilities: x.r.Capabilities, MaxActive: x.r.MaxActive}
	x.s.State.Agents[a.ID] = a
	return a, nil
}
func (x *transition) appoint(a *Agent) {
	l := &x.s.State.Leadership
	l.AgentID = a.ID
	l.Term++
	l.LeaseUntil = x.now.Add(LeaseDuration)
	for _, t := range x.s.State.Tasks {
		if t.State == "offered" || t.State == "queued" {
			t.LeaderID = a.ID
			t.LeaderTerm = l.Term
			t.Version++
			t.UpdatedAt = x.now
		}
	}
	x.response.Leadership = l
	x.event("leader.changed", a.ID, "", "", fmt.Sprintf("%s is leader (term %d)", a.Label, l.Term))
}
func (x *transition) apply() error {
	r := x.r
	s := &x.s.State
	if r.Op == "init" {
		if !isAdmin(x.a) {
			return Fail("forbidden", "workspace owner/admin must initialize coordination")
		}
		if s.Enabled {
			return Fail("conflict", "coordination already initialized")
		}
		p, e := x.join()
		if e != nil {
			return e
		}
		x.r.Role = "leader"
		x.r.MaxActive = 1
		a, e := x.register(p)
		if e != nil {
			return e
		}
		s.Enabled = true
		s.OperatorID = x.a.UserID
		x.response.Peer = p
		x.response.Agent = a
		x.appoint(a)
		return nil
	}
	if !s.Enabled {
		return Fail("not_found", "coordination not initialized; use peer init")
	}
	if r.Op == "join" {
		p, e := x.join()
		if e != nil {
			return e
		}
		x.response.Peer = p
		if r.Name != "" {
			a, e := x.register(p)
			if e != nil {
				return e
			}
			x.response.Agent = a
		}
		x.event("peer.joined", "", "", "", p.SessionName+" joined the peer board")
		return nil
	}
	p, e := x.peer()
	if e != nil {
		return e
	}
	switch r.Op {
	case "register":
		a, e := x.register(p)
		if e != nil {
			return e
		}
		x.response.Agent = a
		x.event("agent.registered", "", "", "", a.Label+" registered as "+a.Role)
		return nil
	case "heartbeat":
		p.LastSeen = x.now
		p.LeaseUntil = x.now.Add(LeaseDuration)
		p.State = "ready"
		if !r.Ready {
			p.State = "paused"
		}
		for _, a := range s.Agents {
			if a.PeerID == p.ID {
				a.Ready = r.Ready && slices.Contains(r.ActiveAgents, a.ID)
			}
		}
		l := &s.Leadership
		if a := s.Agents[l.AgentID]; a != nil && a.PeerID == p.ID && a.Ready && r.Term == l.Term {
			l.LeaseUntil = x.now.Add(LeaseDuration)
		}
		for _, t := range s.Tasks {
			at := t.Current()
			if at != nil && Active(t.State) && at.Incarnation == p.Incarnation && s.Agents[at.AgentID] != nil && s.Agents[at.AgentID].PeerID == p.ID && slices.Contains(r.Running, at.ExecutionID) && x.now.Before(at.LeaseUntil) {
				at.LeaseUntil = x.now.Add(LeaseDuration)
			}
		}
		x.response.Peer = p
		x.response.Leadership = l
		return nil
	case "leave":
		for _, t := range s.Tasks {
			at := t.Current()
			if at != nil && Active(t.State) && s.Agents[at.AgentID] != nil && s.Agents[at.AgentID].PeerID == p.ID {
				return Fail("conflict", "active attempts must drain or be confirmed stopped before leaving")
			}
		}
		p.State = "revoked"
		p.LeaseUntil = x.now
		delete(x.s.PeerSecrets, p.ID)
		for _, a := range s.Agents {
			if a.PeerID == p.ID {
				a.Ready = false
			}
		}
		if a := s.Agents[s.Leadership.AgentID]; a != nil && a.PeerID == p.ID {
			s.Leadership.LeaseUntil = x.now
		}
		x.event("peer.left", "", "", "", p.SessionName+" left the peer board")
		return nil
	case "leader.handover":
		if !isAdmin(x.a) {
			if _, e = x.leader(); e != nil {
				return e
			}
		}
		a, e := s.ResolveAgent(r.Target)
		if e != nil {
			return e
		}
		owner := s.Peers[a.PeerID]
		if a.Role != "leader" || owner == nil || owner.State == "revoked" {
			return Fail("invalid", "target is not an enrolled leader candidate")
		}
		x.appoint(a)
		return nil
	case "leader.standbys":
		if !isAdmin(x.a) {
			return Fail("forbidden", "workspace owner/admin must approve standbys")
		}
		if len(r.Standbys) > 10 {
			return Fail("invalid", "at most ten standbys")
		}
		ids := []string{}
		for _, ref := range r.Standbys {
			a, e := s.ResolveAgent(ref)
			if e != nil {
				return e
			}
			if a.Role != "leader" {
				return Fail("invalid", "standby must be a leader candidate")
			}
			if !slices.Contains(ids, a.ID) {
				ids = append(ids, a.ID)
			}
		}
		s.Leadership.Standbys = ids
		s.Leadership.AutoFailover = r.AutoFailover
		x.response.Leadership = &s.Leadership
		x.event("leader.standbys", "", "", "", "Approved leader standby policy updated")
		return nil
	case "leader.acquire":
		a, e := x.ownAgent()
		if e != nil {
			return e
		}
		l := s.Leadership
		if !l.AutoFailover || x.now.Before(l.LeaseUntil) {
			return Fail("conflict", "automatic failover unavailable or current leader lease remains active")
		}
		chosen := ""
		for _, id := range l.Standbys {
			v := s.Agents[id]
			if v != nil {
				peer := s.Peers[v.PeerID]
				if v.Ready && peer != nil && peer.State == "ready" && x.now.Before(peer.LeaseUntil) {
					chosen = id
					break
				}
			}
		}
		if chosen == "" || chosen != a.ID {
			return Fail("forbidden", "this endpoint is not the first ready approved standby")
		}
		x.appoint(a)
		return nil
	case "board.pause":
		if !isAdmin(x.a) {
			return Fail("forbidden", "workspace owner/admin required")
		}
		s.Paused = r.Paused
		x.event("board.paused", "", "", "", fmt.Sprintf("Board admissions paused=%t", r.Paused))
		return nil
	case "send", "objective":
		a, e := x.ownAgent()
		if e != nil {
			return e
		}
		target, e := s.ResolveAgent(r.Target)
		if e != nil {
			return e
		}
		if !validText(r.Text) {
			return Fail("invalid", "nonempty message up to 16 KiB required")
		}
		task, thread := "", ""
		if r.TaskID != "" {
			t, e := s.ResolveTask(r.TaskID)
			if e != nil {
				return e
			}
			task = t.ID
			thread = t.ThreadID
			if t.AgentID != a.ID && s.Leadership.AgentID != a.ID {
				return Fail("forbidden", "task message requires its worker or current leader")
			}
			if r.AttemptID != "" && (t.Current() == nil || t.Current().ID != r.AttemptID) {
				return Fail("stale", "message refers to an obsolete attempt")
			}
		}
		kind := "message"
		if r.Op == "objective" {
			if target.ID != s.Leadership.AgentID {
				return Fail("invalid", "objective must address current leader")
			}
			kind = "objective"
		}
		x.event(kind, target.ID, task, thread, a.Label+": "+r.Text)
		return nil
	case "task.assign":
		return x.assign()
	}
	if strings.HasPrefix(r.Op, "task.") {
		return x.task()
	}
	return Fail("invalid", "unknown coordination operation %q", r.Op)
}
func (x *transition) assign() error {
	leader, e := x.leader()
	if e != nil {
		return e
	}
	r := x.r
	s := &x.s.State
	target, e := s.ResolveAgent(r.Target)
	if e != nil {
		return e
	}
	if target.Role != "worker" || r.Runner != target.Runner || !slices.Contains(target.Projects, r.Project) {
		return Fail("invalid", "recipient does not offer the requested project/runner")
	}
	if !validText(r.Text) || !validText(r.Criteria) || !ValidName(r.Project) || len(r.Base) > 256 || r.MaxRuntimeSeconds < 30 || r.MaxRuntimeSeconds > 86400 || len(r.Dependencies) > 100 {
		return Fail("invalid", "objective, criteria, project and runtime 30..86400 seconds required")
	}
	pending := 0
	for _, t := range s.Tasks {
		if !Terminal(t.State) {
			pending++
		}
	}
	if pending >= MaxPendingTasks {
		return Fail("capacity", "pending task limit reached")
	}
	deps := []string{}
	ready := true
	for _, ref := range r.Dependencies {
		t, e := s.ResolveTask(ref)
		if e != nil {
			return e
		}
		if !slices.Contains(deps, t.ID) {
			deps = append(deps, t.ID)
		}
		if t.State != "completed" {
			ready = false
		}
	}
	s.NextTask++
	t := &Task{ID: uuid.NewString(), Number: s.NextTask, ThreadID: r.ID, Objective: r.Text, Criteria: r.Criteria, Project: r.Project, Base: r.Base, Runner: r.Runner, AgentID: target.ID, LeaderID: leader.ID, LeaderTerm: s.Leadership.Term, Version: 1, State: "offered", Dependencies: deps, MaxRuntimeSeconds: r.MaxRuntimeSeconds, CreatedAt: x.now, UpdatedAt: x.now}
	if !ready {
		t.State = "queued"
	}
	t.Attempts = []Attempt{{ID: uuid.NewString(), Number: 1, AgentID: target.ID, State: t.State}}
	s.Tasks[t.ID] = t
	x.response.Task = t
	x.event("task.assigned", target.ID, t.ID, t.ThreadID, fmt.Sprintf("%s assigned %s to %s: %s", leader.Label, t.Label(), target.Label, t.Objective))
	return nil
}
func (x *transition) execution(t *Task) (*Attempt, error) {
	a, e := x.ownAgent()
	if e != nil {
		return nil, e
	}
	at := t.Current()
	if at == nil || at.ID != x.r.AttemptID || at.AgentID != a.ID || at.ExecutionID != x.r.ExecutionID || !tokenMatches(x.a.ExecutionToken, x.s.ExecutionSecrets[at.ID]) {
		return nil, Fail("forbidden", "matching task execution credential required")
	}
	if at.Incarnation != x.r.Incarnation && x.r.Op != "task.adopt" {
		return nil, Fail("stale", "execution must be reconciled after daemon rejoin")
	}
	return at, nil
}
func (x *transition) task() error {
	r := x.r
	s := &x.s.State
	t, e := s.ResolveTask(r.TaskID)
	if e != nil {
		return e
	}
	at := t.Current()
	if at == nil {
		return Fail("conflict", "task has no attempt")
	}
	if r.Op == "task.claim" {
		a, e := x.ownAgent()
		if e != nil {
			return e
		}
		p := s.Peers[a.PeerID]
		if t.AgentID != a.ID || r.AttemptID != at.ID {
			return Fail("forbidden", "assignment belongs to another endpoint or attempt")
		}
		if s.Paused || !a.Ready || p.State != "ready" || !x.now.Before(p.LeaseUntil) {
			return Fail("unavailable", "worker is not ready")
		}
		if t.Version != r.TaskVersion || (t.State != "offered" && t.State != "queued") {
			return Fail("conflict", "task version or state no longer claimable")
		}
		if t.LeaderTerm != s.Leadership.Term || !x.now.Before(s.Leadership.LeaseUntil) {
			return Fail("stale", "offer requires a current leader lease")
		}
		for _, id := range t.Dependencies {
			if s.Tasks[id] == nil || s.Tasks[id].State != "completed" {
				return Fail("unavailable", "task dependencies are not complete")
			}
		}
		active := 0
		for _, v := range s.Tasks {
			if v.AgentID == a.ID && Active(v.State) {
				active++
			}
		}
		if active >= a.MaxActive {
			return Fail("capacity", "worker is at capacity")
		}
		if !ValidID(r.ExecutionID) || len(x.a.ExecutionToken) < 32 || len(x.a.ExecutionToken) > 256 {
			return Fail("invalid", "execution ID and credential required")
		}
		at.ExecutionID = r.ExecutionID
		at.Incarnation = r.Incarnation
		at.LeaseUntil = x.now.Add(LeaseDuration)
		x.s.ExecutionSecrets[at.ID] = HashToken(x.a.ExecutionToken)
		return x.change(t, "claimed", "task.claimed", s.Leadership.AgentID, "Assignment claimed")
	}
	switch r.Op {
	case "task.review":
		if _, e = x.leader(); e != nil {
			return e
		}
		if t.State != "result_submitted" || t.Version != r.TaskVersion {
			return Fail("conflict", "only the current submitted result can be reviewed")
		}
		state := "completed"
		if !r.Ready {
			state = "failed"
		}
		return x.change(t, state, "task.reviewed", t.AgentID, r.Text)
	case "task.cancel":
		if _, e = x.leader(); e != nil {
			if !isAdmin(x.a) {
				return e
			}
		}
		if Terminal(t.State) {
			return Fail("conflict", "task already terminal")
		}
		state := "cancel_requested"
		if t.State == "queued" || t.State == "offered" || t.State == "result_submitted" {
			state = "cancelled"
		}
		return x.change(t, state, "task.cancel", t.AgentID, r.Text)
	case "task.resolve":
		if !isAdmin(x.a) || !r.ConfirmStopped {
			return Fail("forbidden", "operator must explicitly confirm that the old execution stopped")
		}
		if !Active(t.State) {
			return Fail("conflict", "only active or uncertain attempts need reconciliation")
		}
		delete(x.s.ExecutionSecrets, at.ID)
		return x.change(t, "failed", "task.resolved", s.Leadership.AgentID, "Operator confirmed execution stopped: "+r.Text)
	case "task.retry":
		if _, e = x.leader(); e != nil {
			return e
		}
		if !Terminal(t.State) || t.State == "completed" {
			return Fail("conflict", "only failed, cancelled or expired tasks can be retried")
		}
		target, e := s.ResolveAgent(r.Target)
		if e != nil {
			return e
		}
		if target.Role != "worker" || target.Runner != t.Runner || !slices.Contains(target.Projects, t.Project) {
			return Fail("invalid", "retry target lacks required project/runner")
		}
		if len(t.Attempts) >= 20 {
			return Fail("capacity", "task retry limit reached")
		}
		delete(x.s.ExecutionSecrets, at.ID)
		t.AgentID = target.ID
		t.LeaderID = s.Leadership.AgentID
		t.LeaderTerm = s.Leadership.Term
		t.Attempts = append(t.Attempts, Attempt{ID: uuid.NewString(), Number: len(t.Attempts) + 1, AgentID: target.ID, State: "offered"})
		return x.change(t, "offered", "task.assigned", target.ID, "Explicit retry of "+t.Label())
	}
	at, e = x.execution(t)
	if e != nil {
		return e
	}
	if Terminal(t.State) || t.State == "result_submitted" {
		return Fail("conflict", "attempt already terminal or submitted")
	}
	switch r.Op {
	case "task.adopt":
		// Adoption never changes execution identity or creates a new attempt.
		if !Active(t.State) {
			return Fail("conflict", "only an active execution can be adopted")
		}
		at.Incarnation = r.Incarnation
		at.LeaseUntil = x.now.Add(LeaseDuration)
		x.response.Task = t
		return nil
	case "task.start":
		if t.State != "claimed" || !x.now.Before(at.LeaseUntil) || !ValidID(r.PaneID) {
			return Fail("conflict", "live claim and task pane required before startup")
		}
		at.PaneID = r.PaneID
		at.StartedAt = &x.now
		return x.change(t, "running", "task.started", s.Leadership.AgentID, "Task agent started")
	case "task.progress":
		if t.State == "cancel_requested" {
			return Fail("conflict", "cancellation pending")
		}
		if !x.now.Before(at.LeaseUntil) {
			return Fail("stale", "execution lease expired; reconcile before further progress")
		}
		state := r.Name
		if state == "" {
			state = t.State
		}
		if state != "running" && state != "blocked" && state != "waiting_local_input" {
			return Fail("invalid", "progress state must be running, blocked or waiting_local_input")
		}
		if !validText(r.Text) {
			return Fail("invalid", "progress detail required")
		}
		return x.change(t, state, "task.progress", s.Leadership.AgentID, r.Text)
	case "task.result":
		if t.State == "cancel_requested" || !x.now.Before(at.LeaseUntil) {
			return Fail("stale", "cancelled or expired execution cannot submit success")
		}
		if len(r.Result) == 0 || len(r.Result) > MaxText || !json.Valid(r.Result) {
			return Fail("invalid", "result must be JSON up to 16 KiB")
		}
		var result struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(r.Result, &result) != nil || !validText(result.Summary) {
			return Fail("invalid", "result must include a nonempty summary")
		}
		at.Result = append(json.RawMessage{}, r.Result...)
		return x.change(t, "result_submitted", "task.result", s.Leadership.AgentID, result.Summary)
	case "task.fail":
		if !validText(r.Text) {
			return Fail("invalid", "failure detail required")
		}
		return x.change(t, "failed", "task.failed", s.Leadership.AgentID, r.Text)
	case "task.cancelled":
		if t.State != "cancel_requested" {
			return Fail("conflict", "no cancellation requested")
		}
		return x.change(t, "cancelled", "task.cancelled", s.Leadership.AgentID, "Local runner confirmed stopped")
	case "task.uncertain":
		return x.change(t, "needs_reconciliation", "task.uncertain", s.Leadership.AgentID, r.Text)
	}
	return Fail("invalid", "unknown task operation %q", r.Op)
}
func (x *transition) change(t *Task, state, kind, to, text string) error {
	if len(text) > MaxText {
		return Fail("invalid", "detail too long")
	}
	t.State = state
	t.Version++
	t.UpdatedAt = x.now
	at := t.Current()
	at.State = state
	at.Detail = text
	if Terminal(state) || state == "result_submitted" {
		at.EndedAt = &x.now
	}
	x.response.Task = t
	x.event(kind, to, t.ID, t.ThreadID, t.Label()+": "+text)
	return nil
}

// VerifyPeer is also checked before returning a cached mutation receipt, so a
// revoked registration cannot regain access through request replay.
func VerifyPeer(store Store, auth Auth, req Request) error {
	x := transition{s: &store, a: auth, r: req}
	_, err := x.peer()
	return err
}
