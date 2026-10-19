// SPDX-License-Identifier: Apache-2.0

// Package peercoord defines the versioned, messages-only coordination protocol.
// Display names are not addresses or authority. The server binds every endpoint
// and execution to an authenticated peer registration.
package peercoord

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const Version = 1
const LeaseDuration = 30 * time.Second
const MaxAgents = 100
const MaxPendingTasks = 500
const MaxText = 16384

type Peer struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	SessionID   string    `json:"session_id"`
	SessionName string    `json:"session_name"`
	Incarnation string    `json:"incarnation"`
	LastSeen    time.Time `json:"last_seen"`
	LeaseUntil  time.Time `json:"lease_until"`
	State       string    `json:"state"`
}
type Agent struct {
	ID           string   `json:"id"`
	PeerID       string   `json:"peer_id"`
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Role         string   `json:"role"`
	Runner       string   `json:"runner"`
	Projects     []string `json:"projects"`
	Capabilities []string `json:"capabilities"`
	MaxActive    int      `json:"max_active"`
	Ready        bool     `json:"ready"`
}
type Leadership struct {
	AgentID      string    `json:"agent_id"`
	Term         int64     `json:"term"`
	LeaseUntil   time.Time `json:"lease_until"`
	Standbys     []string  `json:"standbys"`
	AutoFailover bool      `json:"auto_failover"`
}
type Attempt struct {
	ID          string          `json:"id"`
	Number      int             `json:"number"`
	AgentID     string          `json:"agent_id"`
	ExecutionID string          `json:"execution_id,omitempty"`
	Incarnation string          `json:"incarnation,omitempty"`
	State       string          `json:"state"`
	LeaseUntil  time.Time       `json:"lease_until"`
	PaneID      string          `json:"pane_id,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Detail      string          `json:"detail,omitempty"`
}
type Task struct {
	ID                string    `json:"id"`
	Number            int64     `json:"number"`
	ThreadID          string    `json:"thread_id"`
	Objective         string    `json:"objective"`
	Criteria          string    `json:"criteria"`
	Project           string    `json:"project"`
	Base              string    `json:"base,omitempty"`
	Runner            string    `json:"runner"`
	AgentID           string    `json:"agent_id"`
	LeaderID          string    `json:"leader_id"`
	LeaderTerm        int64     `json:"leader_term"`
	Version           int64     `json:"version"`
	State             string    `json:"state"`
	Dependencies      []string  `json:"dependencies"`
	MaxRuntimeSeconds int       `json:"max_runtime_seconds"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Attempts          []Attempt `json:"attempts"`
}

func (t *Task) Current() *Attempt {
	if len(t.Attempts) == 0 {
		return nil
	}
	return &t.Attempts[len(t.Attempts)-1]
}
func (t Task) Label() string { return fmt.Sprintf("T-%d", t.Number) }
func Terminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "expired":
		return true
	}
	return false
}
func Active(state string) bool {
	switch state {
	case "claimed", "starting", "running", "blocked", "waiting_local_input", "cancel_requested", "needs_reconciliation":
		return true
	}
	return false
}

type State struct {
	SchemaVersion int               `json:"schema_version"`
	Enabled       bool              `json:"enabled"`
	Paused        bool              `json:"paused"`
	OperatorID    string            `json:"operator_id"`
	Leadership    Leadership        `json:"leadership"`
	Peers         map[string]*Peer  `json:"peers"`
	Agents        map[string]*Agent `json:"agents"`
	Tasks         map[string]*Task  `json:"tasks"`
	NextTask      int64             `json:"next_task"`
	ServerTime    time.Time         `json:"server_time"`
}

// Store includes authentication material that must never be returned by a read API.
type Store struct {
	State            State             `json:"state"`
	PeerSecrets      map[string]string `json:"peer_secrets"`
	ExecutionSecrets map[string]string `json:"execution_secrets"`
}

func NewStore() Store {
	return Store{State: State{SchemaVersion: Version, Peers: map[string]*Peer{}, Agents: map[string]*Agent{}, Tasks: map[string]*Task{}}, PeerSecrets: map[string]string{}, ExecutionSecrets: map[string]string{}}
}

type Request struct {
	SchemaVersion     int             `json:"schema_version"`
	ID                string          `json:"id"`
	Op                string          `json:"op"`
	PeerID            string          `json:"peer_id,omitempty"`
	AgentID           string          `json:"agent_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	SessionName       string          `json:"session_name,omitempty"`
	Incarnation       string          `json:"incarnation,omitempty"`
	Name              string          `json:"name,omitempty"`
	Role              string          `json:"role,omitempty"`
	Runner            string          `json:"runner,omitempty"`
	Projects          []string        `json:"projects,omitempty"`
	Capabilities      []string        `json:"capabilities,omitempty"`
	MaxActive         int             `json:"max_active,omitempty"`
	Ready             bool            `json:"ready,omitempty"`
	ActiveAgents      []string        `json:"active_agents,omitempty"`
	Running           []string        `json:"running,omitempty"`
	ConfirmStopped    bool            `json:"confirm_stopped,omitempty"`
	Target            string          `json:"target,omitempty"`
	Term              int64           `json:"term,omitempty"`
	TaskID            string          `json:"task_id,omitempty"`
	AttemptID         string          `json:"attempt_id,omitempty"`
	TaskVersion       int64           `json:"task_version,omitempty"`
	ExecutionID       string          `json:"execution_id,omitempty"`
	PaneID            string          `json:"pane_id,omitempty"`
	Text              string          `json:"text,omitempty"`
	Criteria          string          `json:"criteria,omitempty"`
	Project           string          `json:"project,omitempty"`
	Base              string          `json:"base,omitempty"`
	Dependencies      []string        `json:"dependencies,omitempty"`
	MaxRuntimeSeconds int             `json:"max_runtime_seconds,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	Standbys          []string        `json:"standbys,omitempty"`
	AutoFailover      bool            `json:"auto_failover,omitempty"`
	Paused            bool            `json:"paused,omitempty"`
}

// Auth is supplied by the HTTP/daemon boundary, never decoded from Request.
type Auth struct {
	UserID         string
	WorkspaceRole  string
	PeerToken      string
	ExecutionToken string
}
type Event struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Sequence      int64     `json:"sequence"`
	Kind          string    `json:"kind"`
	SenderID      string    `json:"sender_id,omitempty"`
	RecipientID   string    `json:"recipient_id,omitempty"`
	TaskID        string    `json:"task_id,omitempty"`
	AttemptID     string    `json:"attempt_id,omitempty"`
	ThreadID      string    `json:"thread_id,omitempty"`
	Text          string    `json:"text"`
	CreatedAt     time.Time `json:"created_at"`
}
type Response struct {
	SchemaVersion int         `json:"schema_version"`
	Peer          *Peer       `json:"peer,omitempty"`
	Agent         *Agent      `json:"agent,omitempty"`
	Task          *Task       `json:"task,omitempty"`
	Leadership    *Leadership `json:"leadership,omitempty"`
	Event         *Event      `json:"event,omitempty"`
	Replayed      bool        `json:"replayed"`
	ServerTime    time.Time   `json:"server_time"`
}
type Page struct {
	Events  []Event `json:"events"`
	Next    int64   `json:"next"`
	HasMore bool    `json:"has_more"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                    { return e.Code + ": " + e.Message }
func Fail(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }
func ReadOnlyOp(op string) bool {
	switch op {
	case "state", "events":
		return true
	}
	return false
}
func (s State) ResolveAgent(ref string) (*Agent, error) {
	if a := s.Agents[ref]; a != nil {
		return a, nil
	}
	var found *Agent
	for _, a := range s.Agents {
		if a.Label == ref {
			if found != nil {
				return nil, Fail("ambiguous", "agent name %q is ambiguous; use its registered UUID", ref)
			}
			found = a
		}
	}
	if found == nil {
		return nil, Fail("not_found", "registered agent %q not found", ref)
	}
	return found, nil
}
func (s State) ResolveTask(ref string) (*Task, error) {
	if t := s.Tasks[ref]; t != nil {
		return t, nil
	}
	for _, t := range s.Tasks {
		if strings.EqualFold(t.Label(), ref) {
			return t, nil
		}
	}
	return nil, Fail("not_found", "task %q not found", ref)
}
