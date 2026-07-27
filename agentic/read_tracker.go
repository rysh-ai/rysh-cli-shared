package agentic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// Phase 3 item L — file-read tracking + stale-edit protection.
//
// The model can read a file early in a turn, do other work, then ask to
// edit it many tool calls later. If the user (or another tool) modified the
// file in between, the model's edit instructions are stale and applying
// them clobbers the change. ReadTracker remembers which files the model
// has read (path + mtime + content-sha) and the orchestrator consults it
// before applying file_edit / multi_edit / file_write.
//
// Tracker scope: ONE per OrchestratorActor instance. Sub-agents get a
// fresh tracker — by design. The parent's reads don't matter inside the
// child's isolated context; the child's reads don't matter to the parent.
//
// What's NOT covered (yet):
//   - apply_patch (parses patch headers; deferred to a follow-up).
//   - Files modified inside bash (no path to inspect from outside the
//     tool). Mitigation: any bash that touches files should be approved.

// readRecord captures the state of a file at the moment the model read it.
// Comparing this against the current state on the next edit attempt detects
// out-of-band modification.
type readRecord struct {
	Path   string
	ModSec int64  // file mtime in seconds (resilient to sub-second clock drift)
	SHA256 string // sha256 of file content at read time
	Size   int64
}

// ReadTracker remembers which files were read during an orchestrator run.
// Goroutine-safe: ALL access goes through ReadTracker methods, and the
// orchestrator's runLoop is single-threaded so no external locking is
// required (consistent with the rest of the orchestrator state).
type ReadTracker struct {
	files map[string]readRecord
}

// NewReadTracker returns an empty tracker. Each orchestrator creates one
// at construction time.
func NewReadTracker() *ReadTracker {
	return &ReadTracker{files: make(map[string]readRecord)}
}

// Record stamps the current state of path into the tracker. Called after a
// successful file_read. Errors are silently ignored — the tracker is best-
// effort; a record we couldn't capture just means no stale-edit check will
// fire on that path until the next successful read.
func (t *ReadTracker) Record(path string) {
	if t == nil || path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	stat, err := os.Stat(abs)
	if err != nil || stat.IsDir() {
		return
	}
	hash, err := hashFile(abs)
	if err != nil {
		return
	}
	t.files[abs] = readRecord{
		Path:   abs,
		ModSec: stat.ModTime().Unix(),
		SHA256: hash,
		Size:   stat.Size(),
	}
}

// Refresh re-stamps a path. Called after a successful edit so the next
// edit on the same file doesn't immediately re-fire the stale check.
func (t *ReadTracker) Refresh(path string) { t.Record(path) }

// Forget removes a path from the tracker (e.g. after a write to a brand-new
// location). Useful when a tool re-creates a file from scratch.
func (t *ReadTracker) Forget(path string) {
	if t == nil || path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	delete(t.files, abs)
}

// staleCheck compares a tracked file's current state against the recorded
// state. Returns nil if the file is unchanged (or not tracked), or a
// structured ToolOutput error suitable for direct return when the file
// changed underneath the model.
//
// Logic:
//   - No record → not tracked, pass.
//   - File deleted → return stale error (the model expected it to exist).
//   - mtime + size match → pass (cheap fast-path).
//   - mtime or size differ → recompute sha; if sha matches the record,
//     pass (mtime got bumped without a real change); otherwise fail.
func (t *ReadTracker) staleCheck(path string) *tools.ToolOutput {
	if t == nil || path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	rec, ok := t.files[abs]
	if !ok {
		return nil
	}
	stat, err := os.Stat(abs)
	if err != nil {
		return tools.ErrOutputf(tools.ErrKindStaleRead,
			"stale read protection: %s was read earlier but is now missing — re-read or recreate the file before editing", path)
	}
	curSec := stat.ModTime().Unix()
	if curSec == rec.ModSec && stat.Size() == rec.Size {
		return nil
	}
	// mtime or size moved — confirm via content hash.
	cur, err := hashFile(abs)
	if err != nil {
		return nil // hash failure: give the edit the benefit of the doubt
	}
	if cur == rec.SHA256 {
		// Same content despite touched mtime; refresh and pass.
		rec.ModSec = curSec
		t.files[abs] = rec
		return nil
	}
	return tools.ErrOutputf(tools.ErrKindStaleRead,
		"stale read protection: %s has changed on disk since you read it (size %d→%d). Re-read the file with file_read before editing.",
		path, rec.Size, stat.Size())
}

// hashFile returns the hex-encoded sha256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractFilePath pulls the conventional "file_path" field out of a tool's
// JSON params. Returns "" when absent or malformed — the orchestrator
// treats a missing path as "no tracker action" rather than failing.
//
// Tool authors that want to participate in the read tracker should name
// their primary path argument `file_path` for consistency.
func extractFilePath(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.FilePath
}

// extractFilePaths returns ALL file paths a tool will operate on. For
// `apply_patch` it parses the patch headers and returns the set of
// modify/delete targets (create targets are skipped — nothing on disk
// to be stale about). For other tools it falls back to the single-path
// `extractFilePath`. Follow-up item 7.
func extractFilePaths(toolName string, params json.RawMessage) []string {
	if toolName == "apply_patch" {
		var p struct {
			Patch string `json:"patch"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil
		}
		targets, err := ParsePatchTargets(p.Patch)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(targets))
		for _, t := range targets {
			if t.Kind != PatchCreate {
				out = append(out, t.Path)
			}
		}
		return out
	}
	if p := extractFilePath(params); p != "" {
		return []string{p}
	}
	return nil
}
