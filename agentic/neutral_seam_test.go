// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// Design 002 A1 — the orchestrator's provider boundary now routes through the
// neutral ChatProvider seam (callProvider). These tests prove the seam is
// behavior-preserving: the provider receives EXACTLY the conversation, tools,
// and system prompt it received on the old direct path (i.e. the neutral
// round trip at the seam is the identity), the response comes back unchanged,
// and the streamed flag still mirrors StreamingProvider support.

// seamRecorder is an AgenticProvider that records what reaches it.
type seamRecorder struct {
	resp      *provider.AgenticResponse
	gotConv   []provider.ConversationTurn
	gotTools  []provider.ToolSpec
	gotSystem string
	calls     int
}

func (r *seamRecorder) Name() string { return "seam-recorder" }

func (r *seamRecorder) Complete(context.Context, string) (string, error) { return "", nil }

func (r *seamRecorder) CompleteWithTools(
	_ context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
) (*provider.AgenticResponse, error) {
	r.calls++
	r.gotConv = conversation
	r.gotTools = tools
	r.gotSystem = systemPrompt
	return r.resp, nil
}

// seamStreamRecorder additionally implements StreamingProvider, scripting a
// fixed event sequence to the callback.
type seamStreamRecorder struct {
	seamRecorder
	events      []provider.StreamEvent
	streamCalls int
}

func (r *seamStreamRecorder) CompleteWithToolsStream(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
	cb provider.StreamCallback,
) (*provider.AgenticResponse, error) {
	r.streamCalls++
	for _, ev := range r.events {
		if cb != nil {
			cb(ev)
		}
	}
	return r.seamRecorder.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// seamConversation exercises every turn shape the orchestrator stores.
func seamConversation() []provider.ConversationTurn {
	return []provider.ConversationTurn{
		{Role: "user", Content: "fix the bug", Category: provider.TurnCategoryUser, TimestampMs: 1},
		{
			Role: "assistant", Content: "checking",
			ToolCalls:   []provider.ToolCallRequest{{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
			Thinking:    []provider.ThinkingBlock{{Text: "look around", Signature: "sig"}},
			Category:    provider.TurnCategoryAI,
			TimestampMs: 2,
		},
		{
			Role: "tool", ToolCallID: "c1", Content: "main.go", IsError: false,
			Category: provider.TurnCategoryTool, Origin: "bash", Summary: "bash(ls)", TimestampMs: 3,
		},
	}
}

var seamTools = []provider.ToolSpec{
	{Name: "bash", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)},
}

// TestCallProvider_NonStreaming_SeamIsIdentity: through the neutral seam a
// non-streaming provider receives the identical request triple and its
// response returns unchanged; streamed stays false.
func TestCallProvider_NonStreaming_SeamIsIdentity(t *testing.T) {
	rec := &seamRecorder{resp: &provider.AgenticResponse{
		TextBlocks: []provider.TextBlock{{Text: "answer"}},
		StopReason: provider.StopReasonEndTurn,
		Usage:      provider.Usage{InputTokens: 7, OutputTokens: 3},
	}}
	conv := seamConversation()
	o := &OrchestratorActor{
		prov:         rec,
		ctx:          context.Background(),
		systemPrompt: "you are rysh",
		conversation: conv,
	}

	resp, streamed, err := o.callProvider(seamTools)
	if err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	if streamed {
		t.Error("non-streaming provider must not report streamed")
	}
	if rec.calls != 1 {
		t.Fatalf("provider calls = %d", rec.calls)
	}
	if !reflect.DeepEqual(rec.gotConv, conv) {
		t.Errorf("conversation drifted through the seam:\n want %+v\n  got %+v", conv, rec.gotConv)
	}
	if !reflect.DeepEqual(rec.gotTools, seamTools) || rec.gotSystem != "you are rysh" {
		t.Errorf("tools/system drifted: %+v %q", rec.gotTools, rec.gotSystem)
	}
	if !reflect.DeepEqual(resp, rec.resp) {
		t.Errorf("response drifted:\n want %+v\n  got %+v", rec.resp, resp)
	}
}

// TestCallProvider_Streaming_SeamIsIdentity: a streaming provider is detected
// through the seam (streamed=true, stream path taken exactly once), receives
// the identical request triple, and its text deltas reach the orchestrator's
// output callback (observable via lastOutputNL, which emitOutput updates even
// with no publisher attached).
func TestCallProvider_Streaming_SeamIsIdentity(t *testing.T) {
	rec := &seamStreamRecorder{
		seamRecorder: seamRecorder{resp: &provider.AgenticResponse{
			TextBlocks: []provider.TextBlock{{Text: "hello\n"}},
			StopReason: provider.StopReasonEndTurn,
		}},
		events: []provider.StreamEvent{
			{Type: provider.StreamEventMessageStart},
			{Type: provider.StreamEventTextDelta, Text: "hello"},
			{Type: provider.StreamEventTextDelta, Text: "\n"},
			{Type: provider.StreamEventMessageStop},
		},
	}
	conv := seamConversation()
	o := &OrchestratorActor{
		prov:         rec,
		ctx:          context.Background(),
		systemPrompt: "sys",
		conversation: conv,
	}

	resp, streamed, err := o.callProvider(seamTools)
	if err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	if !streamed {
		t.Error("streaming provider must report streamed=true")
	}
	if rec.streamCalls != 1 {
		t.Fatalf("stream calls = %d (one-shot calls = %d)", rec.streamCalls, rec.calls)
	}
	if !reflect.DeepEqual(rec.gotConv, conv) {
		t.Errorf("conversation drifted through the seam:\n want %+v\n  got %+v", conv, rec.gotConv)
	}
	if !reflect.DeepEqual(resp, rec.resp) {
		t.Errorf("response drifted:\n want %+v\n  got %+v", rec.resp, resp)
	}
	// The trailing "\n" delta must have flowed through the orchestrator's
	// text-delta emission (emitOutput tracks line state before publishing).
	if !o.lastOutputNL {
		t.Error("text deltas did not reach the orchestrator output callback")
	}
}

// TestCallProvider_EphemeralScreenshotThroughSeam: the ephemeral trailing
// screenshot turn (an image content block) survives the neutral seam
// verbatim and the stored conversation stays untouched.
func TestCallProvider_EphemeralScreenshotThroughSeam(t *testing.T) {
	rec := &seamRecorder{resp: &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn}}
	conv := seamConversation()
	shot := provider.ContentBlock{
		Type:   "image",
		Source: &provider.ImageSource{Type: "base64", MediaType: "image/png", Data: "c2hvdA=="},
	}
	o := &OrchestratorActor{
		prov:             rec,
		ctx:              context.Background(),
		conversation:     conv,
		latestScreenshot: &shot,
	}

	if _, _, err := o.callProvider(nil); err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	// The ephemeral turn is stamped with time.Now at build time; normalize
	// the timestamp before comparing against a freshly-built expectation.
	want := o.requestConversation()
	got := append([]provider.ConversationTurn(nil), rec.gotConv...)
	if len(got) > 0 && len(want) > 0 {
		want[len(want)-1].TimestampMs = 0
		got[len(got)-1].TimestampMs = 0
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("screenshot turn drifted through the seam:\n want %+v\n  got %+v", want, got)
	}
	if len(rec.gotConv) != len(conv)+1 {
		t.Fatalf("expected stored+1 turns, got %d", len(rec.gotConv))
	}
	last := rec.gotConv[len(rec.gotConv)-1]
	if len(last.ContentBlocks) != 1 || !reflect.DeepEqual(last.ContentBlocks[0], shot) {
		t.Errorf("ephemeral screenshot block mangled: %+v", last.ContentBlocks)
	}
	if !reflect.DeepEqual(o.conversation, conv) {
		t.Error("stored conversation mutated")
	}
}
