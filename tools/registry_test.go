package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// stubExecutor is a no-op ToolExecutor for registry tests.
type stubExecutor struct{ name string }

func (s stubExecutor) Execute(context.Context, json.RawMessage) (*ToolOutput, error) {
	return &ToolOutput{}, nil
}
func (s stubExecutor) Spec() ToolSpec                        { return ToolSpec{Name: s.name} }
func (s stubExecutor) RequiresApproval(json.RawMessage) bool { return false }

func TestRegistryUnregister(t *testing.T) {
	r := NewToolRegistry()
	r.Register("a", stubExecutor{name: "a"})
	r.Register("b", stubExecutor{name: "b"})

	if _, ok := r.Get("a"); !ok {
		t.Fatalf("expected tool a present")
	}

	if removed := r.Unregister("a"); !removed {
		t.Fatalf("Unregister(a) should report removed=true")
	}
	if _, ok := r.Get("a"); ok {
		t.Fatalf("tool a still present after Unregister")
	}
	if _, ok := r.Get("b"); !ok {
		t.Fatalf("Unregister(a) must not affect b")
	}

	// Unregistering a missing name is a no-op returning false.
	if removed := r.Unregister("missing"); removed {
		t.Fatalf("Unregister(missing) should report removed=false")
	}
}

func TestChildRegistryReadsParentLive(t *testing.T) {
	parent := NewToolRegistry()
	parent.Register("shared", stubExecutor{name: "shared"})

	child := NewChildRegistry(parent)
	child.Register("own", stubExecutor{name: "own"})

	// Child sees both its own and the parent's tools.
	if _, ok := child.Get("own"); !ok {
		t.Fatalf("child missing own tool")
	}
	if _, ok := child.Get("shared"); !ok {
		t.Fatalf("child cannot see parent tool")
	}

	// A tool added to the parent AFTER the child was created is visible live.
	parent.Register("late", stubExecutor{name: "late"})
	if _, ok := child.Get("late"); !ok {
		t.Fatalf("child did not see parent tool added after creation")
	}
	specNames := map[string]bool{}
	for _, s := range child.AllSpecs() {
		specNames[s.Name] = true
	}
	for _, want := range []string{"own", "shared", "late"} {
		if !specNames[want] {
			t.Fatalf("AllSpecs missing %q: %v", want, specNames)
		}
	}

	// Unregistering from the parent removes it from the child live.
	parent.Unregister("late")
	if _, ok := child.Get("late"); ok {
		t.Fatalf("child still sees parent tool after parent Unregister")
	}

	// Own tools override the parent on name collision.
	parent.Register("dup", stubExecutor{name: "dup-parent"})
	child.Register("dup", stubExecutor{name: "dup-child"})
	if e, _ := child.Get("dup"); e.Spec().Name != "dup-child" {
		t.Fatalf("child own tool should override parent, got %q", e.Spec().Name)
	}
	// AllSpecs must not duplicate the overridden name.
	count := 0
	for _, s := range child.AllSpecs() {
		if s.Name == "dup-child" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one 'dup' spec (own override), got %d", count)
	}
}

func TestChildCloneSnapshotsParentLive(t *testing.T) {
	parent := NewToolRegistry()
	parent.Register("a", stubExecutor{name: "a"})
	child := NewChildRegistry(parent)
	child.Register("own", stubExecutor{name: "own"})

	// Enable a tool on the parent, THEN clone the child (simulates a run after
	// ##integration enable): the flat clone must include it.
	parent.Register("enabled", stubExecutor{name: "enabled"})
	run := child.Clone()
	for _, want := range []string{"a", "own", "enabled"} {
		if _, ok := run.Get(want); !ok {
			t.Fatalf("run clone missing %q", want)
		}
	}

	// The clone is a flat, parent-less snapshot: a later parent change must NOT
	// affect an already-created run clone.
	parent.Register("toolate", stubExecutor{name: "toolate"})
	if _, ok := run.Get("toolate"); ok {
		t.Fatalf("run clone should be a stable snapshot, but saw a later parent add")
	}
	// ...but a fresh clone (next run) picks it up.
	if _, ok := child.Clone().Get("toolate"); !ok {
		t.Fatalf("a fresh clone should reflect the latest parent state")
	}
}

// TestSetParentRepoints verifies an agent-style registry can be re-pointed to a
// different parent chain at runtime (the per-prompt scope-inheritance path).
func TestSetParentRepoints(t *testing.T) {
	a := NewToolRegistry()
	a.Register("a", stubExecutor{name: "a"})
	b := NewToolRegistry()
	b.Register("b", stubExecutor{name: "b"})

	child := NewChildRegistry(a)
	child.Register("own", stubExecutor{name: "own"})

	if _, ok := child.Get("a"); !ok {
		t.Fatalf("child should see 'a' via parent a")
	}
	if _, ok := child.Get("b"); ok {
		t.Fatalf("child should not see 'b' before reparent")
	}

	child.SetParent(b)
	if _, ok := child.Get("a"); ok {
		t.Fatalf("child should no longer see 'a' after reparent")
	}
	if _, ok := child.Get("b"); !ok {
		t.Fatalf("child should see 'b' after reparent")
	}
	if _, ok := child.Clone().Get("b"); !ok {
		t.Fatalf("clone should reflect the new parent")
	}
	if _, ok := child.Clone().Get("own"); !ok {
		t.Fatalf("clone should keep own tools across reparent")
	}

	child.SetParent(nil)
	if _, ok := child.Get("b"); ok {
		t.Fatalf("detached child should not see 'b'")
	}
	if _, ok := child.Get("own"); !ok {
		t.Fatalf("detached child keeps own tools")
	}
}

func TestRegistryCloneIsIndependent(t *testing.T) {
	r := NewToolRegistry()
	r.Register("base", stubExecutor{name: "base"})

	clone := r.Clone()
	clone.Register("extra", stubExecutor{name: "extra"})
	clone.Unregister("base")

	// Mutating the clone must not affect the original.
	if _, ok := r.Get("base"); !ok {
		t.Fatalf("original lost 'base' after clone mutation")
	}
	if _, ok := r.Get("extra"); ok {
		t.Fatalf("original gained 'extra' from clone")
	}
	if _, ok := clone.Get("base"); ok {
		t.Fatalf("clone still has 'base' after Unregister")
	}
	if _, ok := clone.Get("extra"); !ok {
		t.Fatalf("clone missing 'extra'")
	}
}

// TestMultiLevelChainCloneFlattens builds a scope-style chain
// global → tab → lane → pane and verifies Clone() flattens the *entire* chain
// (not just the immediate parent), with deeper scopes overriding wider ones and
// mid-chain ancestors read live.
func TestMultiLevelChainCloneFlattens(t *testing.T) {
	global := NewToolRegistry()
	global.Register("g", stubExecutor{name: "g"})
	global.Register("dup", stubExecutor{name: "dup-global"})

	tab := NewChildRegistry(global)
	tab.Register("t", stubExecutor{name: "t"})

	lane := NewChildRegistry(tab)
	lane.Register("l", stubExecutor{name: "l"})

	pane := NewChildRegistry(lane)
	pane.Register("p", stubExecutor{name: "p"})
	pane.Register("dup", stubExecutor{name: "dup-pane"}) // overrides global's "dup"

	run := pane.Clone() // simulates the orchestrator's per-run flatten

	// All four levels must be present in the flattened snapshot.
	for _, name := range []string{"g", "t", "l", "p"} {
		if _, ok := run.Get(name); !ok {
			t.Fatalf("flattened clone missing %q from the chain", name)
		}
	}
	// Deeper scope overrides the wider one.
	if e, _ := run.Get("dup"); e.Spec().Name != "dup-pane" {
		t.Fatalf("expected pane to override global for 'dup', got %q", e.Spec().Name)
	}
	// AllSpecs has no duplicate for the overridden name.
	dupCount := 0
	for _, s := range run.AllSpecs() {
		if s.Name == "dup-pane" {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Fatalf("expected exactly one 'dup' spec after override, got %d", dupCount)
	}

	// A tool added to a MID-chain ancestor (lane) after the pane was created is
	// picked up by a fresh clone (live read through the chain) but not by an
	// already-taken run snapshot (stable per run).
	lane.Register("late", stubExecutor{name: "late"})
	if _, ok := run.Get("late"); ok {
		t.Fatalf("existing run snapshot should not see a later mid-chain add")
	}
	if _, ok := pane.Clone().Get("late"); !ok {
		t.Fatalf("a fresh clone must see the live mid-chain add")
	}
}

// TestMultiLevelChainCloneIsolatesWorkDir verifies cwd isolation still holds when
// a stateful (cloneTooler) tool lives at a wider scope and is reached through a
// multi-level chain: each Clone deep-copies it so per-run SetWorkDir doesn't leak.
func TestMultiLevelChainCloneIsolatesWorkDir(t *testing.T) {
	global := NewToolRegistry()
	global.Register("fake_wd", &fakeWDTool{dir: "/orig"}) // stateful, at the widest scope

	tab := NewChildRegistry(global)
	pane := NewChildRegistry(tab)

	a := pane.Clone()
	b := pane.Clone()
	a.SetWorkDir("/a")
	b.SetWorkDir("/b")

	ea, _ := a.Get("fake_wd")
	eb, _ := b.Get("fake_wd")
	if got := ea.(*fakeWDTool).dir; got != "/a" {
		t.Fatalf("clone a wd = %q, want /a", got)
	}
	if got := eb.(*fakeWDTool).dir; got != "/b" {
		t.Fatalf("clone b wd = %q, want /b (a's SetWorkDir leaked)", got)
	}
	// The shared global tool itself must be untouched (deep-copied, not mutated).
	eg, _ := global.Get("fake_wd")
	if got := eg.(*fakeWDTool).dir; got != "/orig" {
		t.Fatalf("global wd = %q, want /orig (per-run SetWorkDir leaked to the shared parent)", got)
	}
}
