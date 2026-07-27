package agentic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper to write a fresh file under t.TempDir().
func writeTmp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadTracker_RecordAndStaleCheck_NoChange: a recorded file that hasn't
// moved on disk passes the stale check.
func TestReadTracker_RecordAndStaleCheck_NoChange(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "hello\n")
	rt.Record(p)
	if got := rt.staleCheck(p); got != nil {
		t.Errorf("unchanged file should pass: %v", got)
	}
}

// TestReadTracker_StaleCheck_Modified: writing to the file flips the
// stale check to error.
func TestReadTracker_StaleCheck_Modified(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	rt.Record(p)
	// Bump mtime + content.
	time.Sleep(1100 * time.Millisecond) // mtime resolution is seconds
	if err := os.WriteFile(p, []byte("v2 longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := rt.staleCheck(p)
	if got == nil || got.Error == "" {
		t.Errorf("expected stale error, got nil/empty")
	}
}

// TestReadTracker_StaleCheck_TouchedButSameContent: an mtime bump without a
// content change refreshes silently and passes.
func TestReadTracker_StaleCheck_TouchedButSameContent(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	rt.Record(p)
	time.Sleep(1100 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		t.Fatal(err)
	}
	if got := rt.staleCheck(p); got != nil {
		t.Errorf("mtime-only touch should pass after content compare; got %v", got)
	}
}

// TestReadTracker_StaleCheck_Deleted: a file that vanished between record
// and check yields the structured error.
func TestReadTracker_StaleCheck_Deleted(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	rt.Record(p)
	_ = os.Remove(p)
	got := rt.staleCheck(p)
	if got == nil {
		t.Errorf("expected stale error on missing file")
	}
}

// TestReadTracker_NotTracked: an un-recorded path passes (no policy).
func TestReadTracker_NotTracked(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	if got := rt.staleCheck(p); got != nil {
		t.Errorf("untracked file should pass; got %v", got)
	}
}

// TestReadTracker_Refresh: editing through the tool (refresh) re-anchors so
// subsequent edits don't trip on the new state.
func TestReadTracker_Refresh(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	rt.Record(p)
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(p, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt.Refresh(p)
	if got := rt.staleCheck(p); got != nil {
		t.Errorf("after Refresh, check should pass; got %v", got)
	}
}

// TestReadTracker_Forget: removing a path from the tracker re-disables the
// check (used when a tool fully replaces a file).
func TestReadTracker_Forget(t *testing.T) {
	rt := NewReadTracker()
	p := writeTmp(t, "a.txt", "v1\n")
	rt.Record(p)
	rt.Forget(p)
	if got := rt.staleCheck(p); got != nil {
		t.Errorf("Forget should drop the entry; got %v", got)
	}
}

// TestExtractFilePath covers the standard {"file_path": "..."} extraction
// and the malformed/empty fallbacks.
func TestExtractFilePath(t *testing.T) {
	if got := extractFilePath(json.RawMessage(`{"file_path": "/a/b"}`)); got != "/a/b" {
		t.Errorf("standard = %q", got)
	}
	if got := extractFilePath(json.RawMessage(`{"other": "x"}`)); got != "" {
		t.Errorf("missing field should yield empty, got %q", got)
	}
	if got := extractFilePath(json.RawMessage(`{not-json`)); got != "" {
		t.Errorf("bad json should yield empty, got %q", got)
	}
	if got := extractFilePath(nil); got != "" {
		t.Errorf("nil should yield empty, got %q", got)
	}
}

// TestIsStaleCheckedTool pins the gated tool set so the list isn't changed
// silently.
func TestIsStaleCheckedTool(t *testing.T) {
	for _, n := range []string{"edit", "apply_patch"} {
		if !isStaleCheckedTool(n) {
			t.Errorf("%s should be stale-checked", n)
		}
	}
	for _, n := range []string{"file_read", "file_write", "bash", "grep", "ls"} {
		if isStaleCheckedTool(n) {
			t.Errorf("%s should NOT be stale-checked", n)
		}
	}
}
