package agentic

import "strings"

// ANSI SGR codes for diff colouring. Kept local so the renderer has no external
// dependency. The orchestrator injects these only into the *user-facing* emit
// (executeWithPreview); the model's tool_result and the ANSI-stripped shared
// output stream are unaffected, so remote viewers still see plain text.
const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
)

// colorizeDiff wraps a unified diff in ANSI colour so the terminal renders
// additions as green rows, removals as red rows, hunk headers in cyan, and the
// file headers in bold. Lines that are not part of the diff body are passed
// through unchanged. It is idempotent-safe for already-plain text and never
// alters the textual content (only adds escape codes), so a downstream
// ANSI-strip recovers the original diff exactly.
func colorizeDiff(diff string) string {
	if diff == "" {
		return diff
	}
	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			// File headers: bold, not red/green (they begin with +/- too).
			lines[i] = ansiBold + line + ansiReset
		case strings.HasPrefix(line, "@@"):
			lines[i] = ansiCyan + line + ansiReset
		case strings.HasPrefix(line, "+"):
			lines[i] = ansiGreen + line + ansiReset
		case strings.HasPrefix(line, "-"):
			lines[i] = ansiRed + line + ansiReset
		}
	}
	return strings.Join(lines, "\n")
}
