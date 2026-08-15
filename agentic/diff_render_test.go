// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"
)

// TestColorizeDiff verifies additions/removals/hunk-headers get the right ANSI
// codes and that stripping the codes recovers the original text exactly.
func TestColorizeDiff(t *testing.T) {
	diff := "--- a/f.go\n+++ b/f.go\n@@ -1,2 +1,2 @@\n-old line\n+new line\n unchanged\n"
	out := colorizeDiff(diff)

	if !strings.Contains(out, ansiGreen+"+new line"+ansiReset) {
		t.Errorf("addition not green-coloured: %q", out)
	}
	if !strings.Contains(out, ansiRed+"-old line"+ansiReset) {
		t.Errorf("removal not red-coloured: %q", out)
	}
	if !strings.Contains(out, ansiCyan+"@@ -1,2 +1,2 @@"+ansiReset) {
		t.Errorf("hunk header not cyan: %q", out)
	}
	if !strings.Contains(out, ansiBold+"--- a/f.go"+ansiReset) {
		t.Errorf("file header not bold: %q", out)
	}
	// The "+++"/"---" file headers must be bold, NOT mistaken for +/- rows.
	if strings.Contains(out, ansiGreen+"+++") || strings.Contains(out, ansiRed+"---") {
		t.Errorf("file headers must not be coloured as add/remove rows: %q", out)
	}

	// Stripping ANSI must recover the original diff text exactly.
	if stripped := stripANSI(out); stripped != diff {
		t.Errorf("ANSI-strip did not recover original:\n got %q\nwant %q", stripped, diff)
	}

	if colorizeDiff("") != "" {
		t.Error("empty input should return empty")
	}
}

// stripANSI removes CSI SGR sequences for the round-trip assertion.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
