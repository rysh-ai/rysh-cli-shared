package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ToolConfig holds configuration for tool executors.
type ToolConfig struct {
	WorkDir         string
	BashTimeout     time.Duration
	BlockedCommands []string
	MaxFileSize     int64
	BlockedPaths    []string
}

// ToolSpec describes a tool for the LLM.
type ToolSpec struct {
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	Parameters       json.RawMessage `json:"parameters"` // JSON Schema
	RequiresApproval bool            `json:"-"`
}

// ToolOutput is the result of a tool execution.
type ToolOutput struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
	// ErrorKind is a typed classification of an error: validation,
	// missing, permission_denied, timeout, transient, loop_blocked,
	// stale_read, cancelled, internal. Empty when Error is empty (or for
	// pre-existing callers). Follow-up item 4.
	ErrorKind string            `json:"error_kind,omitempty"`
	ExitCode  int               `json:"exit_code,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Error kind constants. Small, behavioural taxonomy — each kind triggers
// a different *code path* in the orchestrator or the model's recovery
// strategy. Resist adding kinds for every nuance; the Error string is
// where sub-categories belong.
const (
	ErrKindValidation       = "validation"        // bad/missing params; model should re-call with corrected args
	ErrKindMissing          = "missing"           // referenced resource not found (file, URL, tool)
	ErrKindPermissionDenied = "permission_denied" // gated by policy / OS / approval-rejection
	ErrKindTimeout          = "timeout"           // exceeded an internal deadline
	ErrKindTransient        = "transient"         // network blip / rate limit / overloaded; safe to retry
	ErrKindLoopBlocked      = "loop_blocked"      // orchestrator's loop-detector fired
	ErrKindStaleRead        = "stale_read"        // stale-edit tracker refused the op
	ErrKindCancelled        = "cancelled"         // context cancelled mid-execution
	ErrKindInternal         = "internal"          // tool bug / panic-recovered / unexpected
)

// ErrOutput is a small constructor for an error-only ToolOutput with a
// typed kind. Tool authors should prefer this over `&ToolOutput{Error:...}`
// so the kind is captured at the source.
func ErrOutput(kind, msg string) *ToolOutput {
	return &ToolOutput{Error: msg, ErrorKind: kind}
}

// ErrOutputf is the printf-flavoured variant of ErrOutput.
func ErrOutputf(kind, format string, args ...interface{}) *ToolOutput {
	return &ToolOutput{Error: fmt.Sprintf(format, args...), ErrorKind: kind}
}

// ToolExecutor is implemented by each tool.
type ToolExecutor interface {
	Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error)
	Spec() ToolSpec
	RequiresApproval(params json.RawMessage) bool
}

// ToolRegistry holds all registered tool executors.
//
// A registry may optionally have a parent. Lookups (Get/AllSpecs/Names) consult
// the registry's own tools first, then fall back to the parent — so a child
// reads its parent *live*. This is how per-pane / per-agent registries stay in
// sync with the shared registry: per-pane tools (which need pane-specific
// handles) live in the child's own map, while shared tools (bash, file, and
// dynamically enabled forge/mcp tools) are read from the live parent. The
// child's own entries override the parent on name collision.
//
// parent is set once at construction (NewChildRegistry) and never mutated, so
// it is safe to read without holding the lock. Clone() always produces a flat,
// parent-less snapshot (see Clone).
type ToolRegistry struct {
	mu        sync.RWMutex
	executors map[string]ToolExecutor
	// parent is an atomic so it can be re-pointed at runtime via SetParent
	// (used for agents whose scope chain depends on the invoking pane) while
	// being read lock-free by Get/AllSpecs/Names/Clone from other goroutines.
	parent atomic.Pointer[ToolRegistry]
}

// NewToolRegistry creates an empty ToolRegistry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		executors: make(map[string]ToolExecutor),
	}
}

// NewChildRegistry creates a registry that delegates lookups to parent for any
// tool it does not hold itself. Register tools into the child to add per-owner
// tools (or to override a parent tool by name); the parent continues to be read
// live, so tools added to or removed from the parent after the child is created
// are immediately visible through the child. parent may be nil (equivalent to
// NewToolRegistry).
func NewChildRegistry(parent *ToolRegistry) *ToolRegistry {
	r := &ToolRegistry{
		executors: make(map[string]ToolExecutor),
	}
	r.parent.Store(parent)
	return r
}

// SetParent re-points this registry's parent (the rest of its scope chain). Used
// for agent/humanoid registries whose effective scope is the invoking pane's
// chain, resolved per prompt. Reads (Get/AllSpecs/Names/Clone) observe the change
// atomically. Pass nil to detach (lookups then see only own tools).
func (r *ToolRegistry) SetParent(parent *ToolRegistry) {
	r.parent.Store(parent)
}

// Register adds a tool executor under the given name.
func (r *ToolRegistry) Register(name string, executor ToolExecutor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executors[name] = executor
}

// Unregister removes the tool executor registered under name. It returns true
// if a tool was removed, false if no tool was registered under that name. It is
// the inverse of Register and is used to retract dynamically-added tools (e.g.
// tools discovered from an MCP server that is later removed).
func (r *ToolRegistry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.executors[name]; !ok {
		return false
	}
	delete(r.executors, name)
	return true
}

// Get retrieves a tool executor by name. Own tools take precedence; if not
// found and a parent is set, the lookup falls back to the (live) parent.
func (r *ToolRegistry) Get(name string) (ToolExecutor, bool) {
	r.mu.RLock()
	e, ok := r.executors[name]
	r.mu.RUnlock()
	if ok {
		return e, true
	}
	if p := r.parent.Load(); p != nil {
		return p.Get(name)
	}
	return nil, false
}

// AllSpecs returns the ToolSpec for every registered tool, sorted by name. When
// a parent is set, its specs are merged in (read live); own tools override the
// parent on name collision.
func (r *ToolRegistry) AllSpecs() []ToolSpec {
	r.mu.RLock()
	specs := make([]ToolSpec, 0, len(r.executors))
	seen := make(map[string]bool, len(r.executors))
	for _, e := range r.executors {
		s := e.Spec()
		seen[s.Name] = true
		specs = append(specs, s)
	}
	r.mu.RUnlock()
	if p := r.parent.Load(); p != nil {
		for _, s := range p.AllSpecs() {
			if !seen[s.Name] {
				seen[s.Name] = true
				specs = append(specs, s)
			}
		}
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Name < specs[j].Name
	})
	return specs
}

// WorkDirAware is implemented by tools whose relative-path resolution is
// anchored to a working directory that can change at runtime. The orchestrator
// calls Registry.SetWorkDir at the start of each run so AI-mode tools follow
// the pane's live shell cwd (e.g. after the user `cd`s in shell mode).
type WorkDirAware interface {
	SetWorkDir(dir string)
}

// cloneTooler is implemented by stateful tools (those carrying a mutable
// working directory) that must be deep-copied when a registry is cloned, so
// per-pane / per-run SetWorkDir mutations don't leak across the shared
// registry. Stateless tools omit it and continue to be shared by reference.
type cloneTooler interface {
	CloneTool() ToolExecutor
}

// Clone creates a flat, parent-less copy of the registry: the **entire** ancestor
// chain (read live) is merged in root-first, so a narrower (deeper) scope's tools
// override wider ones by name. Tools that implement cloneTooler are deep-copied
// (so per-run working-directory changes are isolated); all other tool references
// are shared. The map is independent so new tools can be added without affecting
// the original.
//
// Walking the full chain (not just the immediate parent) is what lets registries
// be nested into a scope hierarchy — e.g. pane → group → lane → tab → global —
// and still flatten correctly into one per-run snapshot. Because each ancestor is
// read live at clone time, cloning a child picks up tools that were added to (or
// removed from) any ancestor since the child was created. This is what makes
// ##integration / ##mcp enable/disable (at any scope) take effect on the next run
// of an existing pane/agent without re-creating it.
func (r *ToolRegistry) Clone() *ToolRegistry {
	clone := &ToolRegistry{
		executors: make(map[string]ToolExecutor),
	}
	// Collect self → parent → … → root, then copy in reverse (root first) so
	// that deeper registries override shallower ones on name collision. parent
	// is immutable after construction, so the walk needs no lock.
	chain := make([]*ToolRegistry, 0, 4)
	for reg := r; reg != nil; reg = reg.parent.Load() {
		chain = append(chain, reg)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		chain[i].copyExecutorsInto(clone)
	}
	return clone
}

// copyExecutorsInto copies this registry's own executors into dst (deep-copying
// cloneTooler tools), overriding any entry already present under the same name.
// It does not follow the parent chain — callers compose parent-then-self.
func (r *ToolRegistry) copyExecutorsInto(dst *ToolRegistry) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, v := range r.executors {
		if c, ok := v.(cloneTooler); ok {
			dst.executors[k] = c.CloneTool()
		} else {
			dst.executors[k] = v
		}
	}
}

// SetWorkDir updates the working directory on every registered tool that
// implements WorkDirAware. Tools that don't are left untouched. A blank dir is
// ignored so callers can pass an unresolved cwd without clobbering defaults.
func (r *ToolRegistry) SetWorkDir(dir string) {
	if dir == "" {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.executors {
		if w, ok := e.(WorkDirAware); ok {
			w.SetWorkDir(dir)
		}
	}
}

// Names returns the sorted list of registered tool names, including the
// parent's (read live) when a parent is set.
func (r *ToolRegistry) Names() []string {
	seen := make(map[string]bool)
	r.mu.RLock()
	for n := range r.executors {
		seen[n] = true
	}
	r.mu.RUnlock()
	if p := r.parent.Load(); p != nil {
		for _, n := range p.Names() {
			seen[n] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DefaultRegistry creates a ToolRegistry and registers each provided executor
// using its Spec().Name as the key.
func DefaultRegistry(executors ...ToolExecutor) *ToolRegistry {
	r := NewToolRegistry()
	for _, e := range executors {
		r.Register(e.Spec().Name, e)
	}
	return r
}
