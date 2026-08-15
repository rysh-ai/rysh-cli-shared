// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeWDTool is a work-directory-aware, cloneable tool used to exercise
// Registry.SetWorkDir and the deep-clone behavior of Clone.
type fakeWDTool struct {
	dir string
}

func (f *fakeWDTool) Execute(context.Context, json.RawMessage) (*ToolOutput, error) {
	return &ToolOutput{Content: f.dir}, nil
}
func (f *fakeWDTool) Spec() ToolSpec                        { return ToolSpec{Name: "fake_wd"} }
func (f *fakeWDTool) RequiresApproval(json.RawMessage) bool { return false }
func (f *fakeWDTool) SetWorkDir(dir string) {
	if dir != "" {
		f.dir = dir
	}
}
func (f *fakeWDTool) CloneTool() ToolExecutor { cp := *f; return &cp }

// statelessTool implements ToolExecutor but is neither WorkDirAware nor
// cloneTooler, so Clone should share it by reference and SetWorkDir skip it.
type statelessTool struct{ calls int }

func (s *statelessTool) Execute(context.Context, json.RawMessage) (*ToolOutput, error) {
	return &ToolOutput{}, nil
}
func (s *statelessTool) Spec() ToolSpec                        { return ToolSpec{Name: "stateless"} }
func (s *statelessTool) RequiresApproval(json.RawMessage) bool { return false }

func TestRegistrySetWorkDir(t *testing.T) {
	r := NewToolRegistry()
	wd := &fakeWDTool{dir: "/a"}
	r.Register("fake_wd", wd)

	r.SetWorkDir("/b")
	if wd.dir != "/b" {
		t.Errorf("SetWorkDir: dir = %q, want /b", wd.dir)
	}

	// Blank dir is ignored (don't clobber a resolved default).
	r.SetWorkDir("")
	if wd.dir != "/b" {
		t.Errorf("SetWorkDir(\"\"): dir = %q, want /b (unchanged)", wd.dir)
	}
}

// TestCloneIsolatesWorkDir is the core safety property: mutating a cloned
// registry's working directory must NOT affect the original (or any sibling
// clone), or one pane's cwd would leak into another.
func TestCloneIsolatesWorkDir(t *testing.T) {
	base := NewToolRegistry()
	base.Register("fake_wd", &fakeWDTool{dir: "/base"})

	cloneA := base.Clone()
	cloneB := base.Clone()
	cloneA.SetWorkDir("/a")
	cloneB.SetWorkDir("/b")

	get := func(r *ToolRegistry) string {
		e, _ := r.Get("fake_wd")
		return e.(*fakeWDTool).dir
	}
	if got := get(base); got != "/base" {
		t.Errorf("base dir = %q, want /base (must be untouched)", got)
	}
	if got := get(cloneA); got != "/a" {
		t.Errorf("cloneA dir = %q, want /a", got)
	}
	if got := get(cloneB); got != "/b" {
		t.Errorf("cloneB dir = %q, want /b", got)
	}
}

// TestCloneSharesStatelessTools confirms tools without CloneTool are still
// shared by reference (no needless copying of stateless executors).
func TestCloneSharesStatelessTools(t *testing.T) {
	base := NewToolRegistry()
	orig := &statelessTool{}
	base.Register("stateless", orig)

	clone := base.Clone()
	got, _ := clone.Get("stateless")
	if got.(*statelessTool) != orig {
		t.Errorf("stateless tool should be shared by reference across Clone")
	}
}
