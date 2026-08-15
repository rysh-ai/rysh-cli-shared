// SPDX-License-Identifier: Apache-2.0

package agentic

import "sync"

// ApprovalMemory is the session's "approve all like this" registry: the set of
// tool+context keys a human answered `yes_always` to (buildApprovalKey).
//
// It is a shared, guarded object rather than a plain map because of WHO touches
// it and WHEN. The registry belongs to the pane's long-lived
// LLMPromptExecutionActor, but the answers arrive inside an OrchestratorActor —
// a fresh one per prompt, running on its own goroutine.
//
// It used to be a `map[string]bool` COPIED into each orchestrator at
// construction. Every `yes_always` was therefore written to a copy that was
// discarded when the turn ended, and the next turn started from the actor's map,
// which nothing ever wrote to (`SetAutoApproval` had no callers). The registry
// the code described as session-scoped lasted exactly one prompt: answer
// "Always", and the same dialog is back on the next turn. In the desktop app,
// with an agent touching a dozen files, that is a dozen dialogs that never stop
// coming.
//
// Sharing the object fixes the lifetime; the mutex is what makes sharing it
// legal, since the orchestrator's goroutine is not the actor's.
//
// Every method is nil-safe: a nil *ApprovalMemory reads as empty and swallows
// writes, so an orchestrator constructed without one (tests, a headless caller)
// behaves exactly as before.
type ApprovalMemory struct {
	mu   sync.RWMutex
	keys map[string]bool
}

// NewApprovalMemory returns an empty registry.
func NewApprovalMemory() *ApprovalMemory {
	return &ApprovalMemory{keys: make(map[string]bool)}
}

// Approved reports whether key was answered `yes_always` in this session.
func (m *ApprovalMemory) Approved(key string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.keys[key]
}

// Approve records a `yes_always` answer. It applies from the next tool call on,
// for the life of the pane's execution actor — not just the current turn.
func (m *ApprovalMemory) Approve(key string) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = make(map[string]bool)
	}
	m.keys[key] = true
}

// Len returns how many keys are remembered. Diagnostics and tests.
func (m *ApprovalMemory) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// ApprovalMemoryFrom builds a registry seeded from a plain map, for callers
// that still hold one.
func ApprovalMemoryFrom(seed map[string]bool) *ApprovalMemory {
	m := NewApprovalMemory()
	for k, v := range seed {
		if v {
			m.keys[k] = true
		}
	}
	return m
}
