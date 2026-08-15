// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
)

// helper builders --------------------------------------------------------

func userT(s string) ConversationTurn { return ConversationTurn{Role: "user", Content: s} }
func asstT(s string) ConversationTurn { return ConversationTurn{Role: "assistant", Content: s} }
func asstTools(ids ...string) ConversationTurn {
	t := ConversationTurn{Role: "assistant", Content: "calling tools"}
	for _, id := range ids {
		t.ToolCalls = append(t.ToolCalls, ToolCallRequest{ID: id, Name: "bash", Input: []byte(`{}`)})
	}
	return t
}
func toolT(id string) ConversationTurn {
	return ConversationTurn{Role: "tool", ToolCallID: id, Content: "ok"}
}

// a realistic transcript with single- and multi-tool rounds
func sampleTranscript() []ConversationTurn {
	return []ConversationTurn{
		userT("do the thing"),
		asstTools("t1"),
		toolT("t1"),
		asstTools("t2", "t3"), // parallel round
		toolT("t2"),
		toolT("t3"),
		asstT("halfway note"),
		userT("keep going"),
		asstTools("t4"),
		toolT("t4"),
		asstT("done"),
	}
}

// tests ------------------------------------------------------------------

func TestHealToolPairing_WellFormedUnchanged(t *testing.T) {
	in := sampleTranscript()
	out := HealToolPairing(in)
	if len(out) != len(in) {
		t.Fatalf("well-formed transcript changed length: %d -> %d", len(in), len(out))
	}
	if v := ValidateToolPairing(out); len(v) != 0 {
		t.Fatalf("violations on well-formed transcript: %v", v)
	}
}

func TestHealToolPairing_SynthesizesMissingResult(t *testing.T) {
	in := []ConversationTurn{
		userT("go"),
		asstTools("t1", "t2"),
		toolT("t1"),
		// t2's result missing (interrupt / splice)
		userT("continue"),
	}
	out := HealToolPairing(in)
	if v := ValidateToolPairing(out); len(v) != 0 {
		t.Fatalf("still invalid after heal: %v", v)
	}
	found := false
	for _, turn := range out {
		if turn.Role == "tool" && turn.ToolCallID == "t2" {
			found = true
			if !turn.IsError || !strings.Contains(turn.Content, "result missing") {
				t.Fatalf("synthetic result malformed: %+v", turn)
			}
		}
	}
	if !found {
		t.Fatal("missing synthetic result for t2")
	}
}

func TestHealToolPairing_DropsOrphanAndDuplicate(t *testing.T) {
	in := []ConversationTurn{
		toolT("ghost"), // orphan at head
		userT("go"),
		asstTools("t1"),
		toolT("t1"),
		toolT("t1"),    // duplicate
		toolT("other"), // orphan for a different id inside the run
		asstT("done"),
	}
	out := HealToolPairing(in)
	if v := ValidateToolPairing(out); len(v) != 0 {
		t.Fatalf("still invalid after heal: %v", v)
	}
	count := 0
	for _, turn := range out {
		if turn.Role == "tool" {
			count++
			if turn.ToolCallID != "t1" {
				t.Fatalf("unexpected surviving tool turn: %+v", turn)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 tool turn, got %d", count)
	}
}

// Property: slicing a transcript at EVERY index (simulating any window/
// compaction/restore cut) and healing must always yield a valid transcript.
func TestHealToolPairing_EverySliceIsRepaired(t *testing.T) {
	full := sampleTranscript()
	for start := 0; start < len(full); start++ {
		for end := start + 1; end <= len(full); end++ {
			slice := append([]ConversationTurn{}, full[start:end]...)
			out := HealToolPairing(HealConversationHead(slice))
			if v := ValidateToolPairing(out); len(v) != 0 {
				t.Fatalf("slice [%d:%d] not repaired: %v", start, end, v)
			}
		}
	}
}

func TestHealToolPairing_DoesNotMutateInput(t *testing.T) {
	in := []ConversationTurn{userT("go"), asstTools("t1"), userT("next")}
	inLen := len(in)
	_ = HealToolPairing(in)
	if len(in) != inLen || in[1].Role != "assistant" || len(in[1].ToolCalls) != 1 {
		t.Fatal("input slice mutated")
	}
}

// buildMessages must emit ONE user message containing ALL tool_result blocks
// of a parallel round, immediately after the assistant message — one message
// per result is rejected by the Messages API.
func TestBuildMessages_ParallelToolRoundMergesResults(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	msgs := c.buildMessages([]ConversationTurn{
		userT("go"),
		asstTools("t1", "t2"),
		toolT("t1"),
		toolT("t2"),
	})
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (user, assistant, merged results), got %d", len(msgs))
	}
	blocks, ok := msgs[2].Content.([]contentBlock)
	if !ok {
		t.Fatalf("results message content has unexpected type %T", msgs[2].Content)
	}
	if len(blocks) != 2 || blocks[0].Type != "tool_result" || blocks[1].Type != "tool_result" {
		t.Fatalf("want 2 tool_result blocks in ONE message, got %+v", blocks)
	}
	ids := map[string]bool{blocks[0].ToolUseID: true, blocks[1].ToolUseID: true}
	if !ids["t1"] || !ids["t2"] {
		t.Fatalf("merged results missing ids: %+v", ids)
	}
}

// buildMessages heals an unanswered tool_use instead of emitting an invalid
// payload (the soft-bricked-checkpoint scenario: `continue` after a splice).
func TestBuildMessages_HealsUnansweredToolUse(t *testing.T) {
	c := &ClaudeAgenticProvider{}
	msgs := c.buildMessages([]ConversationTurn{
		userT("go"),
		asstTools("t1"),
		// no result — corrupted checkpoint
		userT("continue"),
	})
	// want: user, assistant(tool_use), user(tool_result synthetic), user(continue)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %+v", len(msgs), msgs)
	}
	blocks, ok := msgs[2].Content.([]contentBlock)
	if !ok || len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "t1" || !blocks[0].IsError {
		t.Fatalf("expected synthetic tool_result for t1, got %+v", msgs[2].Content)
	}
}
