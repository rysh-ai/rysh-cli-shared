package msg

import (
	"encoding/json"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// TestSessionMemoryReplace_FullFidelityRoundTrip verifies the ##hop fork
// payload preserves everything replay depends on: tool_use/tool_result
// pairing, thinking blocks (signature included), categories, and the pause
// checkpoint. A lossy hop would break the forked pane's next LLM call.
func TestSessionMemoryReplace_FullFidelityRoundTrip(t *testing.T) {
	in := MsgSessionMemoryReplace{
		Turns: []provider.ConversationTurn{
			{Role: "user", Content: "fix the bug", Category: provider.TurnCategoryUser, TimestampMs: 1},
			{
				Role:     "assistant",
				Content:  "Let me look.",
				Category: provider.TurnCategoryAI,
				ToolCalls: []provider.ToolCallRequest{
					{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"go test"}`)},
				},
				Thinking: []provider.ThinkingBlock{{Text: "hmm", Signature: "sig-1"}},
			},
			{
				Role: "tool", ToolCallID: "t1", Content: "ok",
				Category: provider.TurnCategoryTool, Origin: "bash",
				Summary: "bash: go test — ✓ 1 line",
			},
		},
		Paused:       true,
		PausedReason: StoppedReasonCancelled,
		SourcePaneID: "src-pane",
		SourceAlias:  "builder",
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out MsgSessionMemoryReplace
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	if len(out.Turns) != 3 {
		t.Fatalf("want 3 turns, got %d", len(out.Turns))
	}
	asst := out.Turns[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "t1" || string(asst.ToolCalls[0].Input) != `{"command":"go test"}` {
		t.Errorf("tool_use lost: %+v", asst.ToolCalls)
	}
	if len(asst.Thinking) != 1 || asst.Thinking[0].Signature != "sig-1" {
		t.Errorf("thinking block lost: %+v", asst.Thinking)
	}
	toolTurn := out.Turns[2]
	if toolTurn.ToolCallID != "t1" || toolTurn.Category != provider.TurnCategoryTool || toolTurn.Origin != "bash" {
		t.Errorf("tool result metadata lost: %+v", toolTurn)
	}
	if !out.Paused || out.PausedReason != StoppedReasonCancelled {
		t.Errorf("pause checkpoint lost: %+v", out)
	}
	if out.SourcePaneID != "src-pane" || out.SourceAlias != "builder" {
		t.Errorf("provenance lost: %+v", out)
	}
}

// TestSessionMemoryFork_CodecsRegistered verifies the fork messages decode
// through the default codec registry (the bridge path).
func TestSessionMemoryFork_CodecsRegistered(t *testing.T) {
	r := DefaultCodecRegistry()
	for _, tag := range []string{TagGetSessionMemory, TagSessionMemoryReply, TagSessionMemoryReplace} {
		if _, err := r.Decode(tag, []byte(`{}`)); err != nil {
			t.Errorf("codec missing/broken for %s: %v", tag, err)
		}
	}
}
