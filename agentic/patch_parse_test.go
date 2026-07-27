package agentic

import (
	"strings"
	"testing"
)

// TestParsePatchTargets_StandardPrefix exercises the most common diff form
// produced by `git diff`.
func TestParsePatchTargets_StandardPrefix(t *testing.T) {
	patch := `diff --git a/main.go b/main.go
index 0123abc..4567def 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
 func main() {}
`
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "main.go" || got[0].Kind != PatchModify {
		t.Errorf("got %+v", got)
	}
}

// TestParsePatchTargets_NoPrefix accepts `git diff --no-prefix`-style.
func TestParsePatchTargets_NoPrefix(t *testing.T) {
	patch := `--- main.go
+++ main.go
@@ -1 +1,2 @@
 x
+y
`
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "main.go" || got[0].Kind != PatchModify {
		t.Errorf("got %+v", got)
	}
}

// TestParsePatchTargets_Create: `--- /dev/null` → create.
func TestParsePatchTargets_Create(t *testing.T) {
	patch := `--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package main
+func main() {}
`
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "new.go" || got[0].Kind != PatchCreate {
		t.Errorf("got %+v", got)
	}
}

// TestParsePatchTargets_Delete: `+++ /dev/null` → delete.
func TestParsePatchTargets_Delete(t *testing.T) {
	patch := `--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-func main() {}
`
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "old.go" || got[0].Kind != PatchDelete {
		t.Errorf("got %+v", got)
	}
}

// TestParsePatchTargets_MultiFile: three files in one patch, mixed kinds.
func TestParsePatchTargets_MultiFile(t *testing.T) {
	patch := `--- a/a.go
+++ b/a.go
@@ -1 +1,2 @@
 x
+y
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+content
--- a/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-bye
`
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 targets, got %d (%+v)", len(got), got)
	}
	kinds := map[string]PatchKind{}
	for _, t := range got {
		kinds[t.Path] = t.Kind
	}
	if kinds["a.go"] != PatchModify {
		t.Errorf("a.go: %v", kinds["a.go"])
	}
	if kinds["new.go"] != PatchCreate {
		t.Errorf("new.go: %v", kinds["new.go"])
	}
	if kinds["gone.go"] != PatchDelete {
		t.Errorf("gone.go: %v", kinds["gone.go"])
	}
}

// TestParsePatchTargets_TimestampStripped: some diff tools include a tab+
// timestamp in the header path.
func TestParsePatchTargets_TimestampStripped(t *testing.T) {
	patch := "--- a/main.go\t2025-01-01 10:00:00\n+++ b/main.go\t2025-01-01 10:00:01\n@@ -1 +1 @@\n-x\n+y\n"
	got, err := ParsePatchTargets(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "main.go" {
		t.Errorf("got %+v", got)
	}
}

// TestParsePatchTargets_Empty: empty input returns empty slice, no error.
func TestParsePatchTargets_Empty(t *testing.T) {
	got, err := ParsePatchTargets("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

// TestParsePatchTargets_OrphanedPlus: a +++ without a preceding --- is a
// genuine parse error so the orchestrator can refuse to gate on garbage.
func TestParsePatchTargets_OrphanedPlus(t *testing.T) {
	_, err := ParsePatchTargets("+++ b/x.go\n@@ -1 +1 @@\n+y\n")
	if err == nil {
		t.Errorf("expected parse error for orphaned +++ line")
	}
	if !strings.Contains(err.Error(), "without preceding") {
		t.Errorf("error message unexpected: %v", err)
	}
}

// TestExtractFilePaths_ApplyPatch ensures the orchestrator gets the
// stale-checkable paths from a multi-file patch.
func TestExtractFilePaths_ApplyPatch(t *testing.T) {
	patch := `--- a/a.go
+++ b/a.go
@@ -1 +1 @@
-x
+y
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+content
--- a/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-bye
`
	raw := []byte("{\"patch\": " + jsonString(patch) + "}")
	paths := extractFilePaths("apply_patch", raw)
	// new.go (create) is intentionally NOT in the stale-check set.
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths (modify + delete), got %v", paths)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["a.go"] || !seen["gone.go"] {
		t.Errorf("paths missing: %v", paths)
	}
	if seen["new.go"] {
		t.Errorf("create target should be excluded from stale-check: %v", paths)
	}
}

// TestExtractFilePaths_OtherTools falls back to file_path single-extract.
func TestExtractFilePaths_OtherTools(t *testing.T) {
	paths := extractFilePaths("file_edit", []byte(`{"file_path": "x.go"}`))
	if len(paths) != 1 || paths[0] != "x.go" {
		t.Errorf("got %v", paths)
	}
	paths = extractFilePaths("file_edit", []byte(`{}`))
	if len(paths) != 0 {
		t.Errorf("empty file_path should yield empty, got %v", paths)
	}
}

// jsonString quotes a Go string into a JSON string literal.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
