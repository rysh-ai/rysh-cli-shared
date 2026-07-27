package provider

// Tool-use / tool-result pairing invariants for the Anthropic Messages API.
//
// The API rejects any request in which an assistant message's tool_use blocks
// are not answered by tool_result blocks in the IMMEDIATELY following user
// message ("`tool_use` ids were found without `tool_result` blocks immediately
// after"), and likewise rejects tool_result blocks whose tool_use is not in
// the immediately preceding assistant message.
//
// A stored conversation can violate this in several ways: a run interrupted
// between a tool_use append and its results, history compaction or KV-window
// restores slicing between a call and its result, or any future splice site.
// HealToolPairing repairs a transcript at the turn level; buildMessages calls
// it on EVERY request so no caller can emit an invalid payload. It is the
// mid-transcript generalization of CloseDanglingToolCalls (which heals only
// the tail, on interrupt).

// healedToolResultNote is the synthetic result body for a tool call whose real
// result is missing from the transcript.
const healedToolResultNote = "[error kind=unavailable] result missing — the run was " +
	"interrupted or earlier history was compacted; re-issue the call if still needed"

// HealToolPairing returns a transcript in which every assistant tool_use is
// answered by a matching tool turn in the run of consecutive tool turns that
// immediately follows it, and no tool turn exists without such an owner.
//
//   - Missing results are synthesized (IsError=true, healedToolResultNote) in
//     the assistant turn's ToolCalls order, after any real results.
//   - Orphaned tool turns (no owning assistant tool_use immediately before)
//     are dropped.
//   - Duplicate results for the same id keep the first and drop the rest.
//
// Non-tool turns are passed through untouched. The input slice is not mutated.
func HealToolPairing(turns []ConversationTurn) []ConversationTurn {
	out := make([]ConversationTurn, 0, len(turns))
	i := 0
	for i < len(turns) {
		t := turns[i]
		switch {
		case t.Role == "assistant" && len(t.ToolCalls) > 0:
			out = append(out, t)
			wanted := make(map[string]bool, len(t.ToolCalls))
			for _, tc := range t.ToolCalls {
				wanted[tc.ID] = true
			}
			answered := make(map[string]bool, len(t.ToolCalls))
			j := i + 1
			for j < len(turns) && turns[j].Role == "tool" {
				tr := turns[j]
				if wanted[tr.ToolCallID] && !answered[tr.ToolCallID] {
					answered[tr.ToolCallID] = true
					out = append(out, tr)
				}
				// else: orphan for a different call, or a duplicate — drop.
				j++
			}
			for _, tc := range t.ToolCalls {
				if !answered[tc.ID] {
					out = append(out, ConversationTurn{
						Role:        "tool",
						ToolCallID:  tc.ID,
						Content:     healedToolResultNote,
						IsError:     true,
						Category:    TurnCategorySystem,
						TimestampMs: t.TimestampMs,
					})
				}
			}
			i = j
		case t.Role == "tool":
			// A tool turn with no owning assistant tool_use immediately
			// before it — the API would reject it. Drop.
			i++
		default:
			out = append(out, t)
			i++
		}
	}
	return out
}

// ValidateToolPairing reports every pairing violation in a transcript. An
// empty result means buildMessages can emit the conversation safely. Used by
// tests (and available for debug logging).
func ValidateToolPairing(turns []ConversationTurn) []string {
	var violations []string
	i := 0
	for i < len(turns) {
		t := turns[i]
		switch {
		case t.Role == "assistant" && len(t.ToolCalls) > 0:
			answered := make(map[string]bool, len(t.ToolCalls))
			j := i + 1
			for j < len(turns) && turns[j].Role == "tool" {
				answered[turns[j].ToolCallID] = true
				j++
			}
			for _, tc := range t.ToolCalls {
				if !answered[tc.ID] {
					violations = append(violations, "unanswered tool_use "+tc.ID)
				}
			}
			i = j
		case t.Role == "tool":
			violations = append(violations, "orphaned tool_result "+t.ToolCallID)
			i++
		default:
			i++
		}
	}
	return violations
}
