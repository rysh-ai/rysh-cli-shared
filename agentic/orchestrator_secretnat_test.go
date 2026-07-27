package agentic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/secretnat"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

const (
	testStripeKey = "sk_live_4eC39HqLyjWDarjtT1zdp7dc"
	testGithubKey = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
)

// captureTool records the params it was executed with and returns a canned
// output (which deliberately contains a real secret, as e.g. `env` would).
type captureTool struct {
	gotParams json.RawMessage
	output    *tools.ToolOutput
	approval  bool
}

func (c *captureTool) Execute(_ context.Context, params json.RawMessage) (*tools.ToolOutput, error) {
	c.gotParams = append(json.RawMessage(nil), params...)
	return c.output, nil
}
func (c *captureTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "capture", Description: "test", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (c *captureTool) RequiresApproval(json.RawMessage) bool { return c.approval }

func newNATTestSession(t *testing.T) *secretnat.Session {
	t.Helper()
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr.Session("pane-test")
}

// newNATTestOrch builds a bare (state-only) orchestrator: nil publisher →
// emission no-ops, Noop metrics, private registry.
func newNATTestOrch(nat secretnat.SessionHandle, reg *tools.ToolRegistry) *OrchestratorActor {
	return &OrchestratorActor{
		id:            "orch-test",
		paneID:        "pane-test",
		tools:         reg,
		ctx:           context.Background(),
		autoApproved:  map[string]bool{},
		loopThreshold: 3,
		readTracker:   NewReadTracker(),
		metrics:       NoopMetricsSink{},
		nat:           nat,
	}
}

// TestOrchestratorRestoresInputSanitizesOutput is the core tool-seam
// round-trip: the model sends a synthetic token in tool input → the executor
// receives the REAL value; the executor's output contains a real secret →
// the tool_result turn (destined for SessionMemory/KV) carries tokens only.
func TestOrchestratorRestoresInputSanitizesOutput(t *testing.T) {
	s := newNATTestSession(t)
	// Mint the token exactly as a sanitized user turn would have.
	sanitized := s.Sanitize("my key is " + testGithubKey)
	tok := strings.TrimPrefix(sanitized, "my key is ")
	if tok == testGithubKey {
		t.Fatal("token not minted")
	}

	cap := &captureTool{output: &tools.ToolOutput{
		Content:  "ran fine; observed " + testGithubKey + " and also " + testStripeKey,
		Metadata: map[string]string{"file_path": "/tmp/x"},
	}}
	reg := tools.NewToolRegistry()
	reg.Register("capture", cap)

	o := newNATTestOrch(s, reg)
	tc := provider.ToolCallRequest{
		ID:    "call-1",
		Name:  "capture",
		Input: json.RawMessage(`{"command":"use ` + tok + ` now"}`),
	}
	out := o.executeTool(nil, tc)

	// 1. The executor saw the REAL value.
	if !strings.Contains(string(cap.gotParams), testGithubKey) {
		t.Fatalf("executor did not receive restored input: %s", cap.gotParams)
	}
	// 2. tc.Input itself was never mutated (transient restore only).
	if strings.Contains(string(tc.Input), testGithubKey) {
		t.Fatal("tc.Input was mutated with a real value")
	}
	// 3. The output every consumer sees is sanitized — including a secret the
	// session had never seen before (stripe), minted on the way out.
	if strings.Contains(out.Content, testGithubKey) || strings.Contains(out.Content, testStripeKey) {
		t.Fatalf("tool output leaked: %q", out.Content)
	}
	if !strings.Contains(out.Content, tok) {
		t.Fatalf("known token not reused in output: %q", out.Content)
	}
	// 4. The conversation turn (what lands in SessionMemory → KV) is clean.
	turn := o.buildToolResultTurn(tc, out)
	if strings.Contains(turn.Content, testGithubKey) || strings.Contains(turn.Content, testStripeKey) {
		t.Fatalf("tool_result turn leaked: %q", turn.Content)
	}
	// 5. Round trip: the newly minted stripe token restores to the real key.
	if got := s.Restore(turn.Content); !strings.Contains(got, testStripeKey) {
		t.Fatalf("new mapping not restorable: %q", got)
	}
}

// TestOrchestratorToolErrorSanitized: executor errors that echo secret
// material are sanitized before reaching display/metrics/conversation.
func TestOrchestratorToolErrorSanitized(t *testing.T) {
	s := newNATTestSession(t)
	cap := &captureTool{output: &tools.ToolOutput{
		Content: "",
		Error:   "connect failed for postgres://svc:hunter2pass99@db:5432/x",
	}}
	reg := tools.NewToolRegistry()
	reg.Register("capture", cap)
	o := newNATTestOrch(s, reg)

	out := o.executeTool(nil, provider.ToolCallRequest{ID: "c", Name: "capture", Input: json.RawMessage(`{}`)})
	if strings.Contains(out.Error, "hunter2pass99") {
		t.Fatalf("error leaked password: %q", out.Error)
	}
}

// TestOrchestratorNilNATIsNoop: without a session every hook is identity.
func TestOrchestratorNilNATIsNoop(t *testing.T) {
	cap := &captureTool{output: &tools.ToolOutput{Content: "echo " + testStripeKey}}
	reg := tools.NewToolRegistry()
	reg.Register("capture", cap)
	o := newNATTestOrch(nil, reg)

	in := json.RawMessage(`{"x":"` + testStripeKey + `"}`)
	out := o.executeTool(nil, provider.ToolCallRequest{ID: "c", Name: "capture", Input: in})
	if string(cap.gotParams) != string(in) {
		t.Fatalf("nil NAT changed input: %s", cap.gotParams)
	}
	if out.Content != "echo "+testStripeKey {
		t.Fatalf("nil NAT changed output: %q", out.Content)
	}
}

// TestNatDisplayText: display restore is gated on the restore_display knob.
func TestNatDisplayText(t *testing.T) {
	// Default (restore_display off): displayed text keeps tokens.
	s := newNATTestSession(t)
	tok := s.Sanitize(testStripeKey)
	o := newNATTestOrch(s, tools.NewToolRegistry())
	if got := o.natDisplayText("key: " + tok); got != "key: "+tok {
		t.Fatalf("restore_display off must keep tokens: %q", got)
	}

	// restore_display on: displayed text gets real values back.
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, RestoreDisplay: true})
	if err != nil {
		t.Fatal(err)
	}
	s2 := mgr.Session("pane-test")
	tok2 := s2.Sanitize(testStripeKey)
	o2 := newNATTestOrch(s2, tools.NewToolRegistry())
	if got := o2.natDisplayText("key: " + tok2); got != "key: "+testStripeKey {
		t.Fatalf("restore_display on must restore: %q", got)
	}

	// Nil NAT: identity.
	o3 := newNATTestOrch(nil, tools.NewToolRegistry())
	if got := o3.natDisplayText("x"); got != "x" {
		t.Fatalf("nil NAT display: %q", got)
	}
}

// natSpyProv records the conversation that reached the (fake) wire.
type natSpyProv struct {
	seatProv
	lastConv   []provider.ConversationTurn
	lastSystem string
}

func (p *natSpyProv) CompleteWithTools(_ context.Context, conv []provider.ConversationTurn, _ []provider.ToolSpec, system string) (*provider.AgenticResponse, error) {
	p.lastConv = conv
	p.lastSystem = system
	return &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn}, nil
}

// TestLegProviderStaysNATWrapped: with a NAT session installed, every leg —
// default, seat-overridden, and non-overridable — returns a provider that
// sanitizes outbound. This is the regression test for the WithModelEffort
// unwrap bug class.
func TestLegProviderStaysNATWrapped(t *testing.T) {
	s := newNATTestSession(t)
	conv := []provider.ConversationTurn{{Role: "user", Content: "key " + testStripeKey}}

	check := func(t *testing.T, p provider.AgenticProvider, spy *natSpyProv) {
		t.Helper()
		if _, err := p.CompleteWithTools(context.Background(), conv, nil, "sys "+testStripeKey); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(spy.lastSystem, testStripeKey) {
			t.Fatal("system prompt leaked through leg provider")
		}
		for _, turn := range spy.lastConv {
			if strings.Contains(turn.Content, testStripeKey) {
				t.Fatal("conversation leaked through leg provider")
			}
		}
	}

	// Default leg (no seats).
	spy := &natSpyProv{}
	a := &LLMPromptExecutionActor{prov: spy, nat: s}
	check(t, a.legProvider(), spy)

	// Seat-overridden leg: WithModelEffort resolves on the RAW provider and
	// the result is re-wrapped. (seatProv.WithModelEffort returns a plain
	// seatProv copy, so we assert the wrapper's presence structurally.)
	a.runModel, a.runEffort = "claude-haiku-4-5", "low"
	seat := a.legProvider()
	u, ok := seat.(interface {
		Unwrap() provider.AgenticProvider
	})
	if !ok {
		t.Fatal("seat-overridden leg lost the NAT wrapper")
	}
	if inner, ok := u.Unwrap().(*seatProv); !ok || inner.model != "claude-haiku-4-5" || inner.effort != "low" {
		t.Fatalf("seat not applied under the wrapper: %#v", u.Unwrap())
	}

	// Non-overridable provider with seats set: still wrapped.
	spy2 := &natSpyOnly{}
	b := &LLMPromptExecutionActor{prov: spy2, nat: s}
	b.runModel = "claude-haiku-4-5"
	if _, ok := b.legProvider().(interface {
		Unwrap() provider.AgenticProvider
	}); !ok {
		t.Fatal("non-overridable leg lost the NAT wrapper")
	}

	// Nil NAT: identity (existing behavior preserved).
	c := &LLMPromptExecutionActor{prov: spy2}
	if got := c.legProvider(); got != provider.AgenticProvider(spy2) {
		t.Fatal("nil NAT must return the configured provider unchanged")
	}
}

// natSpyOnly implements AgenticProvider WITHOUT ModelEffortOverridable.
type natSpyOnly struct{}

func (p *natSpyOnly) Name() string { return "spy-only" }
func (p *natSpyOnly) Complete(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (p *natSpyOnly) CompleteWithTools(_ context.Context, _ []provider.ConversationTurn, _ []provider.ToolSpec, _ string) (*provider.AgenticResponse, error) {
	return &provider.AgenticResponse{}, nil
}
