package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestSurfaceToolResultContent_NoError: success outputs pass through unchanged.
func TestSurfaceToolResultContent_NoError(t *testing.T) {
	out := &tools.ToolOutput{Content: "hello"}
	if got := surfaceToolResultContent(out); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestSurfaceToolResultContent_KindPrefix verifies the [error kind=X]
// prefix when ErrorKind is set.
func TestSurfaceToolResultContent_KindPrefix(t *testing.T) {
	out := tools.ErrOutput(tools.ErrKindStaleRead, "main.go changed")
	got := surfaceToolResultContent(out)
	if !strings.Contains(got, "[error kind=stale_read]") {
		t.Errorf("missing kind prefix: %q", got)
	}
	if !strings.Contains(got, "main.go changed") {
		t.Errorf("missing error text: %q", got)
	}
}

// TestSurfaceToolResultContent_NoKindFallback: pre-existing callers that
// set only Error get the generic [error] prefix.
func TestSurfaceToolResultContent_NoKindFallback(t *testing.T) {
	out := &tools.ToolOutput{Error: "something broke"}
	got := surfaceToolResultContent(out)
	if !strings.Contains(got, "[error]") {
		t.Errorf("missing generic [error] prefix: %q", got)
	}
	if strings.Contains(got, "kind=") {
		t.Errorf("unexpected kind in: %q", got)
	}
}

// TestSurfaceToolResultContent_ErrorPlusContent appends content after the
// error line so the model gets both signals.
func TestSurfaceToolResultContent_ErrorPlusContent(t *testing.T) {
	out := &tools.ToolOutput{Error: "exit code 1", ErrorKind: tools.ErrKindValidation, Content: "stderr: something missing"}
	got := surfaceToolResultContent(out)
	if !strings.HasPrefix(got, "[error kind=validation]") {
		t.Errorf("expected kind prefix at start: %q", got)
	}
	if !strings.Contains(got, "stderr: something missing") {
		t.Errorf("expected content after error: %q", got)
	}
}

// TestSurfaceToolResultContent_NilSafe: nil result returns "".
func TestSurfaceToolResultContent_NilSafe(t *testing.T) {
	if got := surfaceToolResultContent(nil); got != "" {
		t.Errorf("expected empty for nil, got %q", got)
	}
}

// TestShouldAutoRetry: only `transient` qualifies today.
func TestShouldAutoRetry(t *testing.T) {
	tests := []struct {
		kind string
		want bool
	}{
		{tools.ErrKindTransient, true},
		{tools.ErrKindValidation, false},
		{tools.ErrKindMissing, false},
		{tools.ErrKindPermissionDenied, false},
		{tools.ErrKindTimeout, false},
		{tools.ErrKindLoopBlocked, false},
		{tools.ErrKindStaleRead, false},
		{tools.ErrKindCancelled, false},
		{tools.ErrKindInternal, false},
		{"", false},
		{"unknown_kind", false},
	}
	for _, c := range tests {
		if got := ShouldAutoRetry(c.kind); got != c.want {
			t.Errorf("ShouldAutoRetry(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

// TestErrOutput / ErrOutputf constructors set both fields.
func TestErrOutputConstructors(t *testing.T) {
	a := tools.ErrOutput(tools.ErrKindMissing, "no such file")
	if a.Error != "no such file" || a.ErrorKind != tools.ErrKindMissing {
		t.Errorf("ErrOutput wrong: %+v", a)
	}
	b := tools.ErrOutputf(tools.ErrKindTimeout, "timeout after %s", "60s")
	if b.Error != "timeout after 60s" || b.ErrorKind != tools.ErrKindTimeout {
		t.Errorf("ErrOutputf wrong: %+v", b)
	}
}
