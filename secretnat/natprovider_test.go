// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// spyProvider records the request it receives — the "wire view".
type spyProvider struct {
	name       string
	lastPrompt string
	lastConv   []provider.ConversationTurn
	lastSystem string
}

func (p *spyProvider) Name() string { return p.name }
func (p *spyProvider) Complete(_ context.Context, prompt string) (string, error) {
	p.lastPrompt = prompt
	return "ok", nil
}
func (p *spyProvider) CompleteWithTools(
	_ context.Context,
	conversation []provider.ConversationTurn,
	_ []provider.ToolSpec,
	systemPrompt string,
) (*provider.AgenticResponse, error) {
	p.lastConv = conversation
	p.lastSystem = systemPrompt
	return &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn}, nil
}

// streamingSpy additionally implements StreamingProvider.
type streamingSpy struct{ spyProvider }

func (p *streamingSpy) CompleteWithToolsStream(
	ctx context.Context,
	conversation []provider.ConversationTurn,
	tools []provider.ToolSpec,
	systemPrompt string,
	cb provider.StreamCallback,
) (*provider.AgenticResponse, error) {
	if cb != nil {
		cb(provider.StreamEvent{Type: provider.StreamEventTextDelta, Text: "hi"})
	}
	return p.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// overridableSpy additionally implements ModelEffortOverridable.
type overridableSpy struct {
	spyProvider
	sink *spyProvider // WithModelEffort returns a provider recording here
}

func (p *overridableSpy) WithModelEffort(model, effort string) provider.AgenticProvider {
	p.sink.name = "spy-" + model
	return p.sink
}

// maxTokensSpy additionally implements MaxTokensOverridable, recording the
// cap it was asked for.
type maxTokensSpy struct {
	spyProvider
	gotMaxTokens int
}

func (p *maxTokensSpy) WithMaxTokens(maxTokens int) provider.AgenticProvider {
	p.gotMaxTokens = maxTokens
	return p
}

func natSession(t *testing.T) *Session {
	t.Helper()
	return newTestManager(t, Options{Enabled: true}).Session("pane-1")
}

func secretConversation() []provider.ConversationTurn {
	return []provider.ConversationTurn{
		{Role: "user", Content: "my key is " + stripeKey},
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{
			{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"echo ` + githubKey + `"}`)},
		}},
		{Role: "user", ContentBlocks: []provider.ContentBlock{
			provider.ContentBlockText("block with " + stripeKey),
			provider.ContentBlockImageBase64("image/png", "aWEBhZGZk"),
		}},
	}
}

func assertNoSecrets(t *testing.T, conv []provider.ConversationTurn, system string) {
	t.Helper()
	if strings.Contains(system, stripeKey) {
		t.Fatal("system prompt leaked")
	}
	for i, turn := range conv {
		if strings.Contains(turn.Content, stripeKey) || strings.Contains(turn.Content, githubKey) {
			t.Fatalf("turn %d Content leaked", i)
		}
		for _, b := range turn.ContentBlocks {
			if strings.Contains(b.Text, stripeKey) {
				t.Fatalf("turn %d ContentBlocks leaked", i)
			}
		}
		for _, tc := range turn.ToolCalls {
			if strings.Contains(string(tc.Input), githubKey) {
				t.Fatalf("turn %d ToolCalls leaked", i)
			}
		}
	}
}

func TestNATProviderSanitizesOutbound(t *testing.T) {
	spy := &spyProvider{name: "spy"}
	s := natSession(t)
	w := Wrap(spy, s)

	if w.Name() != "spy" {
		t.Fatalf("Name = %q", w.Name())
	}

	conv := secretConversation()
	_, err := w.CompleteWithTools(context.Background(), conv, nil, "system uses "+stripeKey)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, spy.lastConv, spy.lastSystem)

	// The caller's conversation is never mutated.
	if !strings.Contains(conv[0].Content, stripeKey) {
		t.Fatal("original conversation was mutated")
	}
	if !strings.Contains(string(conv[1].ToolCalls[0].Input), githubKey) {
		t.Fatal("original tool input was mutated")
	}
	// Image blocks pass through untouched.
	if spy.lastConv[2].ContentBlocks[1].Source.Data != "aWEBhZGZk" {
		t.Fatal("image block modified")
	}

	// Single-turn Complete sanitizes too.
	if _, err := w.Complete(context.Background(), "prompt "+githubKey); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spy.lastPrompt, githubKey) {
		t.Fatal("Complete leaked")
	}
}

func TestNATProviderStreamingCapability(t *testing.T) {
	s := natSession(t)

	// Non-streaming inner must NOT gain StreamingProvider.
	w := Wrap(&spyProvider{name: "spy"}, s)
	if _, ok := w.(provider.StreamingProvider); ok {
		t.Fatal("wrapper falsely advertises streaming")
	}

	// Streaming inner keeps the capability, and outbound is sanitized.
	sspy := &streamingSpy{spyProvider{name: "sspy"}}
	ws := Wrap(sspy, s)
	streamer, ok := ws.(provider.StreamingProvider)
	if !ok {
		t.Fatal("streaming capability lost")
	}
	var deltas []string
	_, err := streamer.CompleteWithToolsStream(context.Background(), secretConversation(), nil,
		"sys "+stripeKey, func(ev provider.StreamEvent) {
			deltas = append(deltas, ev.Text)
		})
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, sspy.lastConv, sspy.lastSystem)
	if len(deltas) != 1 || deltas[0] != "hi" {
		t.Fatalf("callback not passed through: %v", deltas)
	}
}

func TestNATProviderWithModelEffortStaysWrapped(t *testing.T) {
	sink := &spyProvider{}
	ospy := &overridableSpy{spyProvider: spyProvider{name: "spy"}, sink: sink}
	s := natSession(t)
	w := Wrap(ospy, s)

	mo, ok := w.(provider.ModelEffortOverridable)
	if !ok {
		t.Fatal("wrapper must forward ModelEffortOverridable")
	}
	seat := mo.WithModelEffort("haiku", "low")
	// The seat-resolved provider must still sanitize (i.e. still be wrapped).
	_, err := seat.CompleteWithTools(context.Background(), secretConversation(), nil, "sys "+stripeKey)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, sink.lastConv, sink.lastSystem)

	// Inner without override support: seat call returns the wrapper itself.
	plain := Wrap(&spyProvider{name: "p"}, s)
	if mo2, ok := plain.(provider.ModelEffortOverridable); ok {
		if got := mo2.WithModelEffort("x", "y"); got == nil {
			t.Fatal("nil from WithModelEffort")
		}
	}
}

// TestNATProviderWithMaxTokensPassThrough: the wrapper forwards the
// per-request MaxTokens seam and re-wraps (the capped seat still sanitizes);
// with a seam-less inner it returns nil per the MaxTokensOverridable
// contract, which the ChatProvider path turns into a loud error — never a
// silently dropped cap.
func TestNATProviderWithMaxTokensPassThrough(t *testing.T) {
	s := natSession(t)

	mspy := &maxTokensSpy{spyProvider: spyProvider{name: "spy"}}
	w := Wrap(mspy, s)
	mt, ok := w.(provider.MaxTokensOverridable)
	if !ok {
		t.Fatal("wrapper must forward MaxTokensOverridable")
	}
	capped := mt.WithMaxTokens(123)
	if capped == nil {
		t.Fatal("capable inner must not yield nil")
	}
	if mspy.gotMaxTokens != 123 {
		t.Fatalf("cap not forwarded to the inner provider: %d", mspy.gotMaxTokens)
	}
	// The capped seat must still sanitize (i.e. still be wrapped).
	if _, err := capped.CompleteWithTools(context.Background(), secretConversation(), nil, "sys "+stripeKey); err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, mspy.lastConv, mspy.lastSystem)

	// Seam-less inner: nil from the seam, loud error from the chat path.
	plain := Wrap(&spyProvider{name: "p"}, s)
	if got := plain.(provider.MaxTokensOverridable).WithMaxTokens(9); got != nil {
		t.Fatalf("seam-less inner must yield nil, got %T", got)
	}
	turns := []provider.Turn{{Role: provider.RoleUser, Blocks: []provider.Block{{Kind: provider.BlockKindText, Text: "hi"}}}}
	if _, err := provider.AsChatProvider(plain).Chat(context.Background(),
		provider.ChatRequest{Turns: turns, MaxTokens: 9}); err == nil {
		t.Fatal("MaxTokens over a seam-less wrapped provider must error loudly")
	}
}

func TestWrapIdentityAndRewrap(t *testing.T) {
	spy := &spyProvider{name: "spy"}
	if got := Wrap(spy, nil); got != provider.AgenticProvider(spy) {
		t.Fatal("nil session must be identity")
	}
	if got := Wrap(nil, natSession(t)); got != nil {
		t.Fatal("nil provider must stay nil")
	}
	// Re-wrapping replaces, never stacks.
	s := natSession(t)
	w1 := Wrap(spy, s)
	w2 := Wrap(w1, s)
	if u, ok := w2.(interface {
		Unwrap() provider.AgenticProvider
	}); !ok || u.Unwrap() != provider.AgenticProvider(spy) {
		t.Fatal("re-wrap stacked decorators")
	}
}
