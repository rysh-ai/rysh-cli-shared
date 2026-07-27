package provider

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// roundTripConversation is a fixture exercising every block kind and every
// field ConversationTurn can express today: plain/multimodal user turns,
// assistant turns with thinking + parallel tool calls, tool results (plain,
// error, with attachments), all image source kinds, metadata, and an unknown
// role. Shared with chat_provider_test.go so the byte-identity tests run over
// the same shapes.
func roundTripConversation() []ConversationTurn {
	return []ConversationTurn{
		{Role: "user", Content: "hello", Category: TurnCategoryUser, TimestampMs: 100},
		{Role: "assistant", Content: "hi there", Category: TurnCategoryAI, TimestampMs: 200},
		{
			Role:    "user",
			Content: "look at this screenshot",
			ContentBlocks: []ContentBlock{
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "aGk="}},
			},
			Category:    TurnCategoryUser,
			TimestampMs: 300,
		},
		{
			// Image-only user turn (no text at all).
			Role: "user",
			ContentBlocks: []ContentBlock{
				{Type: "image", Source: &ImageSource{Type: "url", URL: "https://example.com/x.png"}},
				{Type: "image", Source: &ImageSource{Type: "file_id", FileID: "file_123"}},
			},
		},
		{
			// Mixed text + text-attachment + image, Content non-empty.
			Role:    "user",
			Content: "caption",
			ContentBlocks: []ContentBlock{
				{Type: "text", Text: "[image unavailable]"},
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/jpeg", Data: "eA=="}},
			},
		},
		{
			Role:    "assistant",
			Content: "let me check",
			ToolCalls: []ToolCallRequest{
				{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
				{ID: "call_2", Name: "file_read", Input: json.RawMessage(`{"path":"/tmp/x"}`)},
			},
			Category:    TurnCategoryAI,
			TimestampMs: 400,
			Thinking: []ThinkingBlock{
				{Text: "I should list files first", Signature: "sig-abc"},
				{Text: "then read the file", Signature: "sig-def"},
			},
		},
		{
			Role:        "tool",
			ToolCallID:  "call_1",
			Content:     "total 0\n-rw-r--r-- x",
			Category:    TurnCategoryTool,
			Origin:      "bash",
			Summary:     "bash(ls)",
			TimestampMs: 500,
		},
		{
			Role:       "tool",
			ToolCallID: "call_2",
			Content:    "[error kind=not_found] no such file",
			IsError:    true,
			Category:   TurnCategoryTool,
			Origin:     "file_read",
		},
		{
			// Tool result with attachments (e.g. browser screenshot capture).
			Role:       "tool",
			ToolCallID: "call_3",
			Content:    `{"ok":true}`,
			ContentBlocks: []ContentBlock{
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "c2hvdA=="}},
				{Type: "text", Text: "capture note"},
			},
			Category: TurnCategoryTool,
			Origin:   "browser_action",
		},
		{
			// Tool result with empty content.
			Role:       "tool",
			ToolCallID: "call_4",
		},
		{
			// Assistant thinking-only turn.
			Role:     "assistant",
			Thinking: []ThinkingBlock{{Text: "hmm", Signature: "sig-x"}},
		},
		{
			// Unknown role passes through verbatim (the shim's buildMessages
			// ignores it; conversion must not lose it).
			Role:     "system-note",
			Content:  "synthetic compaction summary",
			Category: TurnCategorySystem,
		},
		{
			// Degenerate image block with nil source.
			Role:          "user",
			Content:       "broken image below",
			ContentBlocks: []ContentBlock{{Type: "image"}},
		},
		{
			// Empty user turn.
			Role: "user",
		},
	}
}

// TestTurnRoundTrip_AllBlockKinds: ConversationTurn -> Turn -> ConversationTurn
// is the identity for every shape ConversationTurn can express today.
func TestTurnRoundTrip_AllBlockKinds(t *testing.T) {
	conv := roundTripConversation()
	got := ConversationFromTurns(TurnsFromConversation(conv))
	if !reflect.DeepEqual(got, conv) {
		for i := range conv {
			if i < len(got) && !reflect.DeepEqual(got[i], conv[i]) {
				t.Errorf("turn %d mismatch:\n want %+v\n  got %+v", i, conv[i], got[i])
			}
		}
		t.Fatalf("round trip not identity (%d turns in, %d out)", len(conv), len(got))
	}
}

// TestTurnRoundTrip_NilAndEmpty: nil stays nil, empty stays empty (and does
// not become nil), so callers can rely on slice identity semantics.
func TestTurnRoundTrip_NilAndEmpty(t *testing.T) {
	if got := ConversationFromTurns(TurnsFromConversation(nil)); got != nil {
		t.Errorf("nil round trip = %#v, want nil", got)
	}
	empty := []ConversationTurn{}
	got := ConversationFromTurns(TurnsFromConversation(empty))
	if got == nil || len(got) != 0 {
		t.Errorf("empty round trip = %#v, want non-nil empty", got)
	}
}

// TestTurnRoundTrip_PropertyRandomized generates seeded random conversations
// over all block kinds (excluding only the two documented canonicalized
// shapes C1/C2, which have their own tests below) and asserts the round trip
// is the identity.
func TestTurnRoundTrip_PropertyRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	str := func(prefix string) string { return fmt.Sprintf("%s-%d", prefix, rng.Intn(1000)) }
	maybe := func() bool { return rng.Intn(2) == 0 }

	randImage := func() *ImageSource {
		switch rng.Intn(4) {
		case 0:
			return &ImageSource{Type: "base64", MediaType: "image/png", Data: str("data")}
		case 1:
			return &ImageSource{Type: "url", URL: str("https://x/")}
		case 2:
			return &ImageSource{Type: "file_id", FileID: str("file")}
		default:
			return nil // degenerate but expressible
		}
	}
	randBlocks := func(role string, hasContent bool) []ContentBlock {
		n := rng.Intn(4)
		var out []ContentBlock
		for i := 0; i < n; i++ {
			// Rule C1 exclusion: a text block may not LEAD ContentBlocks when
			// Content is empty (that shape canonicalizes; no producer emits it).
			if maybe() && (hasContent || len(out) > 0) {
				out = append(out, ContentBlock{Type: "text", Text: str("txt")})
			} else {
				out = append(out, ContentBlock{Type: "image", Source: randImage()})
			}
		}
		return out
	}
	randTurn := func() ConversationTurn {
		ct := ConversationTurn{}
		if maybe() {
			ct.Category = str("cat")
			ct.Origin = str("org")
			ct.Summary = str("sum")
			ct.TimestampMs = int64(rng.Intn(100000))
		}
		switch rng.Intn(3) {
		case 0:
			ct.Role = "user"
			if maybe() {
				ct.Content = str("prompt")
			}
			ct.ContentBlocks = randBlocks(ct.Role, ct.Content != "")
		case 1:
			ct.Role = "assistant"
			if maybe() {
				ct.Content = str("answer")
			}
			for i := rng.Intn(3); i > 0; i-- {
				ct.ToolCalls = append(ct.ToolCalls, ToolCallRequest{
					ID: str("call"), Name: str("tool"),
					Input: json.RawMessage(`{"a":"` + str("v") + `"}`),
				})
			}
			for i := rng.Intn(3); i > 0; i-- {
				ct.Thinking = append(ct.Thinking, ThinkingBlock{Text: str("think"), Signature: str("sig")})
			}
		default:
			ct.Role = "tool"
			ct.ToolCallID = str("call")
			if maybe() {
				ct.Content = str("result")
			}
			ct.IsError = maybe()
			ct.ContentBlocks = randBlocks(ct.Role, true)
		}
		return ct
	}

	for trial := 0; trial < 300; trial++ {
		var conv []ConversationTurn
		for i := rng.Intn(8); i > 0; i-- {
			conv = append(conv, randTurn())
		}
		got := ConversationFromTurns(TurnsFromConversation(conv))
		if !reflect.DeepEqual(got, conv) {
			t.Fatalf("trial %d: round trip not identity\n want %+v\n  got %+v", trial, conv, got)
		}
	}
}

// TestTurnRoundTrip_C1_LeadingTextBlockCanonicalizes pins rule C1: a
// user turn with EMPTY Content whose ContentBlocks lead with a text block
// comes back with that text promoted into Content. The Anthropic wire content
// is unchanged when other blocks remain (the adapter prepends Content as a
// leading text block).
func TestTurnRoundTrip_C1_LeadingTextBlockCanonicalizes(t *testing.T) {
	orig := []ConversationTurn{{
		Role: "user",
		ContentBlocks: []ContentBlock{
			{Type: "text", Text: "lead"},
			{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "eA=="}},
		},
	}}
	got := ConversationFromTurns(TurnsFromConversation(orig))
	want := []ConversationTurn{{
		Role:    "user",
		Content: "lead",
		ContentBlocks: []ContentBlock{
			{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "eA=="}},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("C1 canonicalization:\n want %+v\n  got %+v", want, got)
	}

	// The canonicalized form builds the SAME Anthropic message content.
	c := NewClaudeAgenticProvider("k", "http://x", "m", 1)
	origMsgs, _ := json.Marshal(c.buildMessages(orig))
	gotMsgs, _ := json.Marshal(c.buildMessages(got))
	if string(origMsgs) != string(gotMsgs) {
		t.Fatalf("C1 wire drift:\n want %s\n  got %s", origMsgs, gotMsgs)
	}
}

// TestTurnRoundTrip_C1_SingleTextBlockBecomesStringForm pins the documented
// residual of rule C1: when the leading text block is the ONLY content block,
// the round-tripped turn uses the plain-string Content form, whose Anthropic
// wire shape is the semantically-equivalent string content instead of a
// one-element block array.
func TestTurnRoundTrip_C1_SingleTextBlockBecomesStringForm(t *testing.T) {
	orig := []ConversationTurn{{
		Role:          "user",
		ContentBlocks: []ContentBlock{{Type: "text", Text: "only"}},
	}}
	got := ConversationFromTurns(TurnsFromConversation(orig))
	want := []ConversationTurn{{Role: "user", Content: "only"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("C1 single-text canonicalization:\n want %+v\n  got %+v", want, got)
	}
}

// TestTurnRoundTrip_C2_UnknownContentBlockTypeBecomesText pins rule C2: an
// unrecognized ContentBlock type canonicalizes to a text block (matching the
// Anthropic adapter's wire behavior); an attached Source is dropped.
func TestTurnRoundTrip_C2_UnknownContentBlockTypeBecomesText(t *testing.T) {
	orig := []ConversationTurn{{
		Role:    "user",
		Content: "prompt",
		ContentBlocks: []ContentBlock{
			{Type: "mystery", Text: "payload", Source: &ImageSource{Type: "base64", Data: "eA=="}},
		},
	}}
	got := ConversationFromTurns(TurnsFromConversation(orig))
	want := []ConversationTurn{{
		Role:          "user",
		Content:       "prompt",
		ContentBlocks: []ContentBlock{{Type: "text", Text: "payload"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("C2 canonicalization:\n want %+v\n  got %+v", want, got)
	}
}

// TestConversationFromTurns_MultiResultFanOut: a neutral tool turn carrying
// several tool_result blocks fans out into one shim turn per result, with
// following attachment blocks landing on their result's turn.
func TestConversationFromTurns_MultiResultFanOut(t *testing.T) {
	img := &ImageSource{Type: "base64", MediaType: "image/png", Data: "eA=="}
	turn := Turn{
		Role:     RoleTool,
		Category: TurnCategoryTool,
		Origin:   "bash",
		Blocks: []Block{
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "one"}},
			{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c2", Content: "two", IsError: true}},
			{Kind: BlockKindImage, Image: img},
		},
	}
	got := ConversationFromTurns([]Turn{turn})
	want := []ConversationTurn{
		{Role: "tool", ToolCallID: "c1", Content: "one", Category: TurnCategoryTool, Origin: "bash"},
		{
			Role: "tool", ToolCallID: "c2", Content: "two", IsError: true,
			Category: TurnCategoryTool, Origin: "bash",
			ContentBlocks: []ContentBlock{{Type: "image", Source: img}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fan-out:\n want %+v\n  got %+v", want, got)
	}
}

// TestChatResponseRoundTrip: AgenticResponse -> ChatResponse -> AgenticResponse
// is the identity, and the neutral blocks carry the canonical order
// (thinking, text, tool calls).
func TestChatResponseRoundTrip(t *testing.T) {
	ar := &AgenticResponse{
		TextBlocks: []TextBlock{{Text: "first"}, {Text: "second"}},
		ToolCalls: []ToolCallRequest{
			{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
			{ID: "c2", Name: "grep", Input: json.RawMessage(`{"q":"x"}`)},
		},
		ThinkingBlocks: []ThinkingBlock{{Text: "think", Signature: "sig"}},
		StopReason:     StopReasonToolUse,
		Usage:          Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 3, CacheCreationInputTokens: 2},
	}
	cr := ChatResponseFromAgentic(ar)
	wantKinds := []BlockKind{BlockKindThinking, BlockKindText, BlockKindText, BlockKindToolCall, BlockKindToolCall}
	if len(cr.Blocks) != len(wantKinds) {
		t.Fatalf("blocks = %+v", cr.Blocks)
	}
	for i, k := range wantKinds {
		if cr.Blocks[i].Kind != k {
			t.Errorf("block %d kind = %q, want %q", i, cr.Blocks[i].Kind, k)
		}
	}
	back := AgenticResponseFromChat(cr)
	if !reflect.DeepEqual(back, ar) {
		t.Fatalf("response round trip:\n want %+v\n  got %+v", ar, back)
	}
}

// TestAgenticResponseFromChat_OutOfContractBlocks pins the documented
// handling of blocks a shim response cannot express: tool_result blocks
// surface their Content as text; image blocks surface their (empty) Text —
// converted, never silently dropped. Nil in, nil out for both converters.
func TestAgenticResponseFromChat_OutOfContractBlocks(t *testing.T) {
	cr := &ChatResponse{Blocks: []Block{
		{Kind: BlockKindToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "res"}},
		{Kind: BlockKindImage, Image: &ImageSource{Type: "url", URL: "https://x"}},
	}}
	got := AgenticResponseFromChat(cr)
	want := &AgenticResponse{TextBlocks: []TextBlock{{Text: "res"}, {Text: ""}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("out-of-contract blocks:\n want %+v\n  got %+v", want, got)
	}
	if ChatResponseFromAgentic(nil) != nil || AgenticResponseFromChat(nil) != nil {
		t.Error("nil responses must convert to nil")
	}
}
