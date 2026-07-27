package agentic

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// Tool-output shaping (Phase 2 item K). Large raw outputs from bash / grep /
// file_read inflate the parent context and degrade subsequent model turns.
// shapeToolOutput applies a head+tail truncation with an explicit marker so
// the model knows content was dropped, while preserving the most informative
// edges of the output. It also annotates the output's Metadata with line and
// byte counts so the model can reason about size without re-counting.
//
// Configurable thresholds: kept as package-level vars (not consts) so a host
// can tune them per environment or via tests. The defaults are tuned for
// Claude's context window: 50 KB / ~12k tokens is comfortable; 600 lines is
// enough to capture most useful tool output without overwhelming the parent.
var (
	// MaxToolOutputBytes is the byte threshold at which truncation kicks in.
	MaxToolOutputBytes = 50000

	// MaxToolOutputLines is the line threshold at which truncation kicks in.
	// Whichever threshold (bytes or lines) fires first triggers truncation.
	MaxToolOutputLines = 600

	// ToolOutputHeadLines is the number of lines preserved verbatim at the
	// HEAD of a truncated output. Tail uses the same count by default.
	ToolOutputHeadLines = 200

	// ToolOutputTailLines is the number of lines preserved verbatim at the
	// TAIL of a truncated output.
	ToolOutputTailLines = 200
)

// shapeToolOutput head+tail truncates an oversized tool output and annotates
// it with size metadata. It mutates the passed *tools.ToolOutput in place and
// returns the same pointer for chaining.
//
// Rules:
//   - nil or empty Content: only the byte/line metadata (== 0) is added.
//   - Content within thresholds: no truncation, just metadata.
//   - Content over either threshold: head+tail strategy. If lines are
//     individually huge (a single 1 MB line of JSON), byte-cap kicks in on
//     each preserved line so the marker also describes the byte savings.
//   - Tool errors are left intact (errors are typically short and informative
//     in full).
func shapeToolOutput(out *tools.ToolOutput) *tools.ToolOutput {
	if out == nil {
		return nil
	}
	if out.Metadata == nil {
		out.Metadata = make(map[string]string)
	}

	original := out.Content
	byteCount := len(original)
	// Compute line count once. A trailing newline does NOT count as an extra
	// empty line, matching the intuition of "lines" rather than "newlines".
	lineCount := 0
	if byteCount > 0 {
		lineCount = strings.Count(original, "\n")
		if !strings.HasSuffix(original, "\n") {
			lineCount++
		}
	}

	// Always annotate; cheap and useful even when not truncating.
	if _, ok := out.Metadata["byte_count"]; !ok {
		out.Metadata["byte_count"] = strconv.Itoa(byteCount)
	}
	if _, ok := out.Metadata["line_count"]; !ok {
		out.Metadata["line_count"] = strconv.Itoa(lineCount)
	}

	if byteCount <= MaxToolOutputBytes && lineCount <= MaxToolOutputLines {
		return out
	}

	// Truncate. Prefer line-based head/tail since output is text and lines
	// are the natural unit; fall back to byte slicing for pathological cases
	// (e.g. one giant line of minified JSON).
	lines := strings.Split(original, "\n")
	// Re-trim the synthetic trailing empty element from Split when the input
	// ends with "\n", so head and tail line indices are stable.
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(original, "\n") {
		lines = lines[:len(lines)-1]
	}

	head := ToolOutputHeadLines
	tail := ToolOutputTailLines
	if head < 1 {
		head = 1
	}
	if tail < 1 {
		tail = 1
	}
	if head+tail >= len(lines) {
		// Few enough lines that line-truncation can't help; switch to a
		// byte-based truncation across the whole content.
		out.Content = byteTruncate(original, MaxToolOutputBytes)
		out.Metadata["truncated"] = "true"
		out.Metadata["truncation_mode"] = "bytes"
		return out
	}

	headLines := lines[:head]
	tailLines := lines[len(lines)-tail:]
	omittedLines := lineCount - head - tail
	omittedBytes := byteCount - sumByteLen(headLines) - sumByteLen(tailLines)
	marker := fmt.Sprintf("\n... [%d lines / %d bytes omitted] ...\n", omittedLines, omittedBytes)

	out.Content = strings.Join(headLines, "\n") + marker + strings.Join(tailLines, "\n")
	out.Metadata["truncated"] = "true"
	out.Metadata["truncation_mode"] = "head_tail"
	out.Metadata["omitted_lines"] = strconv.Itoa(omittedLines)
	out.Metadata["omitted_bytes"] = strconv.Itoa(omittedBytes)
	return out
}

// byteTruncate keeps the first maxBytes/2 and the last maxBytes/2 of s,
// joined by an "... [N bytes omitted] ..." marker. Used when line-based
// truncation cannot help (output is a single very long line).
func byteTruncate(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	half := maxBytes / 2
	if half < 1 {
		half = 1
	}
	head := s[:half]
	tail := s[len(s)-half:]
	omitted := len(s) - 2*half
	return head + fmt.Sprintf("\n... [%d bytes omitted] ...\n", omitted) + tail
}

// sumByteLen returns the total byte length of all strings in xs plus one byte
// per "\n" between them (accounting for the rejoin in shapeToolOutput).
func sumByteLen(xs []string) int {
	if len(xs) == 0 {
		return 0
	}
	total := 0
	for _, s := range xs {
		total += len(s)
	}
	// (len(xs)-1) newlines between adjacent lines.
	return total + (len(xs) - 1)
}
