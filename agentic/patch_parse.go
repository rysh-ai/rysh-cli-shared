package agentic

import (
	"bufio"
	"fmt"
	"strings"
)

// Follow-up item 7 — patch-header parser used to extend stale-edit
// protection to `apply_patch`. The parser scans a unified diff and
// extracts (path, kind) tuples so the orchestrator can run the
// stale-edit tracker against each target before the executor applies.
//
// Why this lives in rysh-shared/agentic and not in rysh-cli/internal/tools:
// the orchestrator needs to call this BEFORE invoking the executor, and
// rysh-shared cannot import rysh-cli. The parser is small (~50 LOC) and
// pure — duplicating it here is preferable to a layering inversion.

// PatchKind classifies a target file in a unified diff.
type PatchKind string

const (
	PatchModify PatchKind = "modify"
	PatchCreate PatchKind = "create"
	PatchDelete PatchKind = "delete"
)

// PatchTarget is one file touched by a unified diff.
type PatchTarget struct {
	Path string
	Kind PatchKind
}

// ParsePatchTargets scans a unified diff and returns the set of files it
// touches with their classification. Both standard `a/`/`b/` prefixes
// and `--no-prefix` form are accepted. Lines that don't match a header
// pair are ignored — this is a *best-effort* extractor; the executor
// remains responsible for validating that the patch actually applies.
//
// Behavior:
//   - "--- a/x" + "+++ b/x"             → modify(x)
//   - "--- /dev/null" + "+++ b/x"       → create(x)
//   - "--- a/x" + "+++ /dev/null"       → delete(x)
//   - any other pairing → skipped
//
// Returns an empty slice (not nil) when the patch is empty or has no
// recognised headers. Errors are reserved for genuine parse failures
// (e.g. a `+++` with no preceding `---`).
func ParsePatchTargets(patch string) ([]PatchTarget, error) {
	if strings.TrimSpace(patch) == "" {
		return []PatchTarget{}, nil
	}
	scanner := bufio.NewScanner(strings.NewReader(patch))
	// Patches can have long lines (e.g. a single very long context line).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		targets   []PatchTarget
		curMinus  string
		haveMinus bool
	)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "--- "):
			curMinus = stripPatchPathPrefix(strings.TrimSpace(line[4:]))
			haveMinus = true
		case strings.HasPrefix(line, "+++ "):
			if !haveMinus {
				return nil, fmt.Errorf("patch: `+++` line without preceding `---`")
			}
			plus := stripPatchPathPrefix(strings.TrimSpace(line[4:]))
			haveMinus = false
			t, ok := classifyTarget(curMinus, plus)
			if ok {
				targets = append(targets, t)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("patch: read: %w", err)
	}
	return targets, nil
}

// stripPatchPathPrefix removes the conventional `a/` or `b/` prefix from
// a header path. Also handles git's "i/x" and "w/x" forms (index/working).
// Leaves `/dev/null` untouched (used to detect create/delete).
func stripPatchPathPrefix(p string) string {
	// Some diff tools include a trailing timestamp; clip at the first tab.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return p
	}
	for _, prefix := range []string{"a/", "b/", "i/", "w/", "c/", "o/"} {
		if strings.HasPrefix(p, prefix) {
			return p[len(prefix):]
		}
	}
	return p
}

// classifyTarget pairs a `---` path with its `+++` path into one
// PatchTarget. Returns (zero, false) if the pairing is unrecognisable
// (e.g. both /dev/null).
func classifyTarget(minus, plus string) (PatchTarget, bool) {
	switch {
	case minus == "/dev/null" && plus != "/dev/null" && plus != "":
		return PatchTarget{Path: plus, Kind: PatchCreate}, true
	case plus == "/dev/null" && minus != "/dev/null" && minus != "":
		return PatchTarget{Path: minus, Kind: PatchDelete}, true
	case minus != "" && plus != "" && minus != "/dev/null" && plus != "/dev/null":
		// Modify. When the two paths differ (a rename), report the
		// destination — that's what stale-check / permission policy will
		// care about.
		path := plus
		return PatchTarget{Path: path, Kind: PatchModify}, true
	}
	return PatchTarget{}, false
}
