package agentic

// The reported failure, reproduced as a transcript: one model answers "I'm
// ChatGPT", `##llm select` swaps to another, the user asks the same question,
// and the second model — reading a stranger's answer in its own voice —
// "corrects itself". These pin the hand-off note that prevents it.

import (
	"context"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

var (
	gpt   = turnAttribution{providerName: "openai", model: "gpt-5.6-luna"}
	opus  = turnAttribution{providerName: "anthropic", model: "claude-opus-4-8"}
	fable = turnAttribution{providerName: "anthropic", model: "claude-fable-5"}
)

func user(text string) provider.ConversationTurn {
	return provider.ConversationTurn{Role: "user", Content: text, Category: provider.TurnCategoryUser}
}

func assistant(a turnAttribution, text string) provider.ConversationTurn {
	return provider.ConversationTurn{
		Role: "assistant", Content: text, Category: provider.TurnCategoryAI,
		ProviderName: a.providerName, Model: a.model,
	}
}

// notes returns the indices and contents of the inserted hand-off turns.
func notes(conv []provider.ConversationTurn) []provider.ConversationTurn {
	var out []provider.ConversationTurn
	for _, t := range conv {
		if t.Origin == modelSwitchOrigin {
			out = append(out, t)
		}
	}
	return out
}

// TestNoteInsertedAtTheSwitch is the reported case: the incoming model is told,
// before it reads the question, that the answer above is not its own.
func TestNoteInsertedAtTheSwitch(t *testing.T) {
	conv := []provider.ConversationTurn{
		user("what model are you?"),
		assistant(gpt, "I'm ChatGPT, an AI assistant operating within the Rysh coding environment."),
		user("what model are you?"),
	}

	got := insertModelSwitchNotes(conv, opus)
	if len(got) != len(conv)+1 {
		t.Fatalf("got %d turns, want %d (one note)", len(got), len(conv)+1)
	}
	// Placement is the whole point: immediately after the foreign answer and
	// BEFORE the user's next question, so it reads in chronological order.
	if got[2].Origin != modelSwitchOrigin {
		t.Fatalf("note landed at the wrong index; turns: %v", roles(got))
	}
	if got[1].Content != conv[1].Content || got[3].Content != conv[2].Content {
		t.Error("insertion disturbed the surrounding turns")
	}
	body := got[2].Content
	for _, want := range []string{"openai (gpt-5.6-luna)", "anthropic (claude-opus-4-8)", "not yours"} {
		if !strings.Contains(body, want) {
			t.Errorf("note does not mention %q:\n%s", want, body)
		}
	}
	// It must not arrive in the assistant's voice — that would be one more turn
	// the model could mistake for its own.
	if got[2].Role != "user" || got[2].Category != provider.TurnCategorySystem {
		t.Errorf("note role/category = %q/%q, want user/system", got[2].Role, got[2].Category)
	}
}

// TestStoredTranscriptIsNotMutated: notes go on the outgoing copy only, so a
// note is never compounded across requests and the session JSON stays a record
// of what each model actually said.
func TestStoredTranscriptIsNotMutated(t *testing.T) {
	conv := []provider.ConversationTurn{
		user("q"), assistant(gpt, "a"), user("q2"),
	}
	before := len(conv)
	got := insertModelSwitchNotes(conv, opus)
	if len(conv) != before {
		t.Fatalf("stored transcript grew to %d", len(conv))
	}
	if &got[0] == &conv[0] {
		t.Error("returned the stored backing array; a later append would corrupt it")
	}
	// Re-running on the same stored transcript yields the same single note.
	if again := insertModelSwitchNotes(conv, opus); len(again) != len(got) {
		t.Errorf("second build produced %d turns, first produced %d", len(again), len(got))
	}
}

// TestNoNoteWhenTheModelNeverChanged: the common case must be free — same
// slice back, no allocation, nothing added to the prompt.
func TestNoNoteWhenTheModelNeverChanged(t *testing.T) {
	conv := []provider.ConversationTurn{
		user("q"), assistant(opus, "a"), user("q2"), assistant(opus, "a2"), user("q3"),
	}
	got := insertModelSwitchNotes(conv, opus)
	if len(got) != len(conv) {
		t.Fatalf("inserted %d notes into a single-model conversation", len(got)-len(conv))
	}
	if &got[0] != &conv[0] {
		t.Error("copied the conversation when there was nothing to say")
	}
}

// TestNoteAtEveryHandoff: two switches, two notes, each naming the pair it sits
// between — including the middle one, where neither side is the active model.
func TestNoteAtEveryHandoff(t *testing.T) {
	conv := []provider.ConversationTurn{
		user("q1"), assistant(gpt, "a1"),
		user("q2"), assistant(fable, "a2"),
		user("q3"),
	}
	got := notes(insertModelSwitchNotes(conv, opus))
	if len(got) != 2 {
		t.Fatalf("got %d notes, want 2", len(got))
	}
	if !strings.Contains(got[0].Content, "openai (gpt-5.6-luna)") ||
		!strings.Contains(got[0].Content, "anthropic (claude-fable-5)") {
		t.Errorf("first hand-off names the wrong pair:\n%s", got[0].Content)
	}
	if !strings.Contains(got[1].Content, "anthropic (claude-fable-5)") ||
		!strings.Contains(got[1].Content, "anthropic (claude-opus-4-8)") {
		t.Errorf("second hand-off names the wrong pair:\n%s", got[1].Content)
	}
}

// TestConsecutiveTurnsFromOneModelAreOneRun: a multi-turn tool loop must not
// produce a note between its own turns.
func TestConsecutiveTurnsFromOneModelAreOneRun(t *testing.T) {
	conv := []provider.ConversationTurn{
		user("q"),
		assistant(gpt, "let me look"),
		{Role: "tool", Content: "result", Category: provider.TurnCategoryTool},
		assistant(gpt, "here it is"),
		user("q2"),
	}
	if got := notes(insertModelSwitchNotes(conv, gpt)); len(got) != 0 {
		t.Fatalf("got %d notes within one model's own tool loop", len(got))
	}
	if got := notes(insertModelSwitchNotes(conv, opus)); len(got) != 1 {
		t.Fatalf("got %d notes after switching away, want exactly 1", len(got))
	}
}

// TestUnattributedTurnsAreNotClaimed: a transcript recorded before attribution
// existed cannot be split into runs honestly. Staying quiet is right; inventing
// a boundary — or letting the new model treat those turns as its own — is not.
func TestUnattributedTurnsAreNotClaimed(t *testing.T) {
	legacy := provider.ConversationTurn{Role: "assistant", Content: "old answer", Category: provider.TurnCategoryAI}
	conv := []provider.ConversationTurn{user("q"), legacy, user("q2")}

	if got := notes(insertModelSwitchNotes(conv, opus)); len(got) != 0 {
		t.Errorf("got %d notes for an unattributed transcript, want 0", len(got))
	}
	if legacy.ProducedBy("anthropic", "claude-opus-4-8") {
		t.Error("an unattributed turn claimed ownership")
	}
	if legacy.ProducedBy("", "") {
		t.Error("an unattributed turn matched the empty model")
	}
}

// TestUnknownActiveModelStaysQuiet: a provider that reports no model at all
// cannot be compared against, so no hand-off is announced rather than one that
// names a blank.
func TestUnknownActiveModelStaysQuiet(t *testing.T) {
	conv := []provider.ConversationTurn{user("q"), assistant(gpt, "a"), user("q2")}
	if got := notes(insertModelSwitchNotes(conv, turnAttribution{})); len(got) != 0 {
		t.Errorf("announced a hand-off to an unknown model: %d notes", len(got))
	}
}

// TestAttributionLabel covers the rendering used in every note.
func TestAttributionLabel(t *testing.T) {
	cases := []struct{ p, m, want string }{
		{"openai", "gpt-5.6-luna", "openai (gpt-5.6-luna)"},
		{"ollama", "", "ollama"},
		{"", "some-model", "some-model"},
		{"", "", ""},
	}
	for _, c := range cases {
		got := provider.ConversationTurn{ProviderName: c.p, Model: c.m}.Attribution()
		if got != c.want {
			t.Errorf("Attribution(%q,%q) = %q, want %q", c.p, c.m, got, c.want)
		}
	}
}

func roles(conv []provider.ConversationTurn) []string {
	out := make([]string, len(conv))
	for i, t := range conv {
		out[i] = t.Role
	}
	return out
}

// ---------------------------------------------------------------------------
// Model identity — "which llm model are you?"
// ---------------------------------------------------------------------------

// TestIdentityBlockNamesTheModel: rysh knows which model it is calling, so the
// model should not have to say it cannot tell.
func TestIdentityBlockNamesTheModel(t *testing.T) {
	got := modelIdentityBlock(gpt)
	for _, want := range []string{"openai", `"gpt-5.6-luna"`, "authoritative"} {
		if !strings.Contains(got, want) {
			t.Errorf("identity block missing %q:\n%s", want, got)
		}
	}
}

// TestRequestSystemPromptAppendsIdentity keeps the caller's prompt intact and
// adds the identity after it.
func TestRequestSystemPromptAppendsIdentity(t *testing.T) {
	o := &OrchestratorActor{systemPrompt: "You are a coding assistant."}
	o.prov = stubModelProvider{name: "openai", model: "gpt-5.6-luna"}

	got := o.requestSystemPrompt()
	if !strings.HasPrefix(got, "You are a coding assistant.") {
		t.Errorf("caller's system prompt was not preserved:\n%s", got)
	}
	if !strings.Contains(got, `model "gpt-5.6-luna"`) {
		t.Errorf("identity absent:\n%s", got)
	}
}

// TestRequestSystemPromptFollowsTheActiveModel is the reason this is composed
// per call: `##llm select` swaps the provider on a live orchestrator, and a
// prompt built once at construction would keep naming the old model.
func TestRequestSystemPromptFollowsTheActiveModel(t *testing.T) {
	o := &OrchestratorActor{systemPrompt: "base"}
	o.prov = stubModelProvider{name: "openai", model: "gpt-5.6-luna"}
	if !strings.Contains(o.requestSystemPrompt(), "gpt-5.6-luna") {
		t.Fatal("precondition: first model not named")
	}

	o.prov = stubModelProvider{name: "anthropic", model: "claude-opus-4-8"}
	got := o.requestSystemPrompt()
	if strings.Contains(got, "gpt-5.6-luna") {
		t.Errorf("still naming the previous model after a switch:\n%s", got)
	}
	if !strings.Contains(got, "claude-opus-4-8") {
		t.Errorf("new model not named:\n%s", got)
	}
}

// TestRequestSystemPromptMakesNoClaimItCannotSupport: a provider that does not
// report a model gets no identity line rather than one naming a blank.
func TestRequestSystemPromptMakesNoClaimItCannotSupport(t *testing.T) {
	o := &OrchestratorActor{systemPrompt: "base"}
	o.prov = stubNoModelProvider{}
	if got := o.requestSystemPrompt(); got != "base" {
		t.Errorf("invented an identity for a provider that reports no model:\n%s", got)
	}
}

// stubModelProvider reports a name and a model, like every real provider.
type stubModelProvider struct{ name, model string }

func (s stubModelProvider) Name() string  { return s.name }
func (s stubModelProvider) Model() string { return s.model }
func (s stubModelProvider) Complete(context.Context, string) (string, error) { return "", nil }
func (s stubModelProvider) CompleteWithTools(context.Context, []provider.ConversationTurn,
	[]provider.ToolSpec, string) (*provider.AgenticResponse, error) {
	return nil, nil
}

// stubNoModelProvider reports a name but no model — the shape of a provider
// that predates Model() or cannot answer it.
type stubNoModelProvider struct{}

func (stubNoModelProvider) Name() string { return "mystery" }
func (stubNoModelProvider) Complete(context.Context, string) (string, error) { return "", nil }
func (stubNoModelProvider) CompleteWithTools(context.Context, []provider.ConversationTurn,
	[]provider.ToolSpec, string) (*provider.AgenticResponse, error) {
	return nil, nil
}

// identityRecorder is a seamRecorder that also reports a model, so the identity
// block has something to name.
type identityRecorder struct {
	seamRecorder
	providerName string
	model        string
}

func (r *identityRecorder) Name() string  { return r.providerName }
func (r *identityRecorder) Model() string { return r.model }

// TestCallProviderSendsTheIdentity pins the CALL SITE, not just the composer.
// Testing requestSystemPrompt alone passes happily while callProvider still
// sends the raw o.systemPrompt — which is exactly the regression that would put
// "I don't have a way to verify the exact model" back in front of the user.
func TestCallProviderSendsTheIdentity(t *testing.T) {
	rec := &identityRecorder{
		seamRecorder: seamRecorder{resp: &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn}},
		providerName: "openai",
		model:        "gpt-5.6-luna",
	}
	o := &OrchestratorActor{
		prov:         rec,
		ctx:          context.Background(),
		systemPrompt: "You are a coding assistant.",
		conversation: []provider.ConversationTurn{user("which llm model are you?")},
	}

	if _, _, err := o.callProvider(nil); err != nil {
		t.Fatalf("callProvider: %v", err)
	}
	if !strings.Contains(rec.gotSystem, `model "gpt-5.6-luna"`) {
		t.Errorf("the request did not carry the model identity:\n%s", rec.gotSystem)
	}
	if !strings.HasPrefix(rec.gotSystem, "You are a coding assistant.") {
		t.Errorf("the caller's system prompt was dropped:\n%s", rec.gotSystem)
	}
	// The stored prompt must stay clean: composing per call means the identity
	// is derived each time, never accumulated onto the field.
	if strings.Contains(o.systemPrompt, "Model identity") {
		t.Error("identity leaked into the stored system prompt; it would compound per call")
	}
	if _, _, err := o.callProvider(nil); err != nil {
		t.Fatalf("second callProvider: %v", err)
	}
	if n := strings.Count(rec.gotSystem, "Model identity"); n != 1 {
		t.Errorf("identity appears %d times after two calls, want 1", n)
	}
}
