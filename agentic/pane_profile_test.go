// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// fakeTool is a named no-op tool.
type fakeTool struct{ name string }

func (f fakeTool) Execute(context.Context, json.RawMessage) (*tools.ToolOutput, error) {
	return &tools.ToolOutput{Content: "ran " + f.name}, nil
}
func (f fakeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: f.name, Description: f.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (f fakeTool) RequiresApproval(json.RawMessage) bool { return false }

func regOf(names ...string) *tools.ToolRegistry {
	r := tools.NewToolRegistry()
	for _, n := range names {
		r.Register(n, fakeTool{n})
	}
	return r
}

func TestBuildProfileRegistry_OnlyAllowedTools(t *testing.T) {
	global := regOf("bash", "file_read", "grep", "edit")
	pane := tools.NewChildRegistry(global) // a pane registry is a live child of its scope chain
	pane.Register("todo", fakeTool{"todo"})
	optIn := regOf("family_ask", "family_reply")

	got := buildProfileRegistry("p1", pane, optIn, []string{"family_ask", "grep", "todo", "no_such_tool"})
	if want := []string{"family_ask", "grep", "todo"}; !reflect.DeepEqual(got.Names(), want) {
		t.Fatalf("names = %v, want %v", got.Names(), want)
	}
	for _, denied := range []string{"bash", "file_read", "edit", "family_reply"} {
		if _, ok := got.Get(denied); ok {
			t.Errorf("%s reachable through the profile registry", denied)
		}
	}
	// The orchestrator clones the registry it is given: the clone must stay narrow.
	if !reflect.DeepEqual(got.Clone().Names(), got.Names()) {
		t.Errorf("clone widened the profile registry: %v", got.Clone().Names())
	}
}

func TestTurnTools_NoProfileIsThePaneRegistry(t *testing.T) {
	pane := regOf("bash", "file_read")
	a := &LLMPromptExecutionActor{paneID: "p1", tools: pane}
	a.SetOptInTools(regOf("family_status"))
	if a.turnTools() != pane {
		t.Fatal("a pane without a profile must use its registry unchanged")
	}
	if _, ok := a.turnTools().Get("family_status"); ok {
		t.Fatal("an opt-in tool leaked into a pane without a profile")
	}
}

func TestSetPaneAgentProfile_SetAndClear(t *testing.T) {
	pane := regOf("bash", "file_read", "grep")
	a := &LLMPromptExecutionActor{paneID: "p1", tools: pane, systemPrompt: "BASE PROMPT"}
	a.SetEnvBlockProvider(func() string { return "ENV BLOCK cwd=/x" })
	a.SetOptInTools(regOf("family_reply"))

	a.handleSetPaneAgentProfile(&msg.MsgSetPaneAgentProfile{
		SystemPrompt: "  You are Grandma Ada.  ",
		AllowTools:   []string{" family_reply ", "grep", "grep", ""},
	})
	if got, _ := a.AgentProfile(); !reflect.DeepEqual(got, []string{"family_reply", "grep"}) {
		t.Fatalf("allow-list not normalized: %v", got)
	}
	if names := a.turnTools().Names(); !reflect.DeepEqual(names, []string{"family_reply", "grep"}) {
		t.Fatalf("profiled turn tools = %v", names)
	}
	sp := a.buildEffectiveSystemPrompt()
	if !strings.HasPrefix(sp, ProfilePreamble) {
		t.Error("profiled system prompt lost the rysh preamble")
	}
	if !strings.Contains(sp, "You are Grandma Ada.") {
		t.Error("profile prompt missing")
	}
	if strings.Contains(sp, "BASE PROMPT") || strings.Contains(sp, "ENV BLOCK") {
		t.Errorf("profiled prompt kept the default prompt or env block: %q", sp)
	}

	// Empty profile restores the defaults.
	a.handleSetPaneAgentProfile(&msg.MsgSetPaneAgentProfile{})
	if a.turnTools() != pane {
		t.Error("clear did not restore the pane registry")
	}
	if sp := a.buildEffectiveSystemPrompt(); sp != "BASE PROMPT\n\nENV BLOCK cwd=/x" {
		t.Errorf("clear did not restore the default prompt: %q", sp)
	}
	// nil is a clear too.
	a.handleSetPaneAgentProfile(&msg.MsgSetPaneAgentProfile{AllowTools: []string{"grep"}})
	a.handleSetPaneAgentProfile(nil)
	if tl, p := a.AgentProfile(); len(tl) != 0 || p != "" {
		t.Errorf("nil profile did not clear: %v %q", tl, p)
	}
}

func TestProfileGroundingMode(t *testing.T) {
	a := &LLMPromptExecutionActor{}
	if got := a.profileGroundingMode(GroundingEnforced); got != GroundingEnforced {
		t.Errorf("no profile: %q", got)
	}
	a.profileTools = []string{"family_ask"}
	if got := a.profileGroundingMode(GroundingEnforced); got != "" {
		t.Errorf("profile without grounding_report must turn grounding off, got %q", got)
	}
	a.profileTools = []string{"grep", GroundingToolName}
	if got := a.profileGroundingMode(GroundingPrompt); got != GroundingPrompt {
		t.Errorf("profile allowing grounding_report keeps the mode, got %q", got)
	}
}

func TestSetPaneAgentProfile_CodecRoundTrip(t *testing.T) {
	r := msg.DefaultCodecRegistry()
	in := &msg.MsgSetPaneAgentProfile{SystemPrompt: "p", AllowTools: []string{"a", "b"}}
	if tag := r.TagOf(in); tag != msg.TagSetPaneAgentProfile {
		t.Fatalf("tag = %q", tag)
	}
	b, _ := json.Marshal(in)
	out, err := r.Decode(msg.TagSetPaneAgentProfile, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip: %#v", out)
	}
}

// specSpy records the tool names and system prompt of every provider call.
type specSpy struct {
	calls chan spyCall
}

type spyCall struct {
	tools  []string
	system string
}

func (s *specSpy) Name() string                                     { return "spec-spy" }
func (s *specSpy) Complete(context.Context, string) (string, error) { return "", nil }
func (s *specSpy) CompleteWithTools(_ context.Context, _ []provider.ConversationTurn, specs []provider.ToolSpec, system string) (*provider.AgenticResponse, error) {
	names := make([]string, 0, len(specs))
	for _, sp := range specs {
		names = append(names, sp.Name)
	}
	s.calls <- spyCall{names, system}
	return &provider.AgenticResponse{TextBlocks: []provider.TextBlock{{Text: "ok"}}, StopReason: provider.StopReasonEndTurn}, nil
}

// TestProfiledPaneTurn_EndToEnd drives a real LLMPromptExecutionActor over an
// embedded NATS: the profile arrives on the pane's llm inbox (codec + bridge),
// the next turn's provider call carries ONLY the allowed tools and the
// profile's prompt, and a clear restores the defaults on the turn after.
func TestProfiledPaneTurn_EndToEnd(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	spy := &specSpy{calls: make(chan spyCall, 8)}
	a := NewLLMPromptExecutionActor("pane-prof", 5, pub, nc, spy, regOf("bash", "file_read", "edit"), "BASE PROMPT", "", "", nil)
	a.SetOptInTools(regOf("family_reply", "family_status"))

	sys := actor.NewActorSystem()
	pid := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return a }))
	defer sys.Root.Stop(pid)

	inbox := msg.T("pane", "pane-prof", "llm_prompt_execution", "inbox")
	// Wait for the bridge subscription (created in Started).
	deadline := time.Now().Add(5 * time.Second)
	for ns.NumSubscriptions() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("actor never subscribed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	turn := func(prompt string) spyCall {
		t.Helper()
		if err := pub.Send(inbox, &msg.MsgAgenticPrompt{Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
		_ = nc.Flush()
		select {
		case c := <-spy.calls:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("no provider call")
		}
		return spyCall{}
	}
	waitIdle := func() {
		t.Helper()
		// the orchestrator finishes asynchronously; give its Done time to land
		time.Sleep(300 * time.Millisecond)
	}

	if err := pub.Send(inbox, &msg.MsgSetPaneAgentProfile{
		SystemPrompt: "You are Grandpa Bo. Answer with family_reply.",
		AllowTools:   []string{"family_reply", "family_status", "not_a_tool"},
	}); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	c := turn("hello")
	if want := []string{"family_reply", "family_status"}; !reflect.DeepEqual(c.tools, want) {
		t.Fatalf("profiled turn offered %v, want %v (bash must be absent)", c.tools, want)
	}
	if !strings.HasPrefix(c.system, ProfilePreamble) || !strings.Contains(c.system, "You are Grandpa Bo.") ||
		strings.Contains(c.system, "BASE PROMPT") {
		t.Fatalf("profiled system prompt wrong: %q", c.system)
	}
	waitIdle()

	if err := pub.Send(inbox, &msg.MsgSetPaneAgentProfile{}); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	c = turn("hello again")
	if want := []string{"bash", "edit", "file_read"}; !reflect.DeepEqual(c.tools, want) {
		t.Fatalf("cleared turn offered %v, want the defaults %v", c.tools, want)
	}
	if !strings.HasPrefix(c.system, "BASE PROMPT") {
		t.Fatalf("cleared system prompt: %q", c.system)
	}
}

// e20 W2: list_tools on an allow-list describes exactly the turn's registry,
// not the pane's full one.
func TestBuildProfileRegistry_ListToolsListsOnlyTheTurnRegistry(t *testing.T) {
	pane := regOf("bash", "file_write", "grep")
	pane.Register(listToolsName, tools.NewListToolsTool(pane)) // as rysh-cli binds it
	r := buildProfileRegistry("p1", pane, regOf("family_reply"), []string{"family_reply", listToolsName})
	e, ok := r.Get(listToolsName)
	if !ok {
		t.Fatal("list_tools not in the profile registry")
	}
	out, err := e.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "Available tools (2):\n\n- family_reply: family_reply\n- list_tools: List all available tools and their descriptions.\n"
	if out.Content != want {
		t.Fatalf("list_tools output:\n%s\nwant exactly:\n%s", out.Content, want)
	}
	// The orchestrator clones the registry each run: the clone's list_tools
	// still lists only the allow-list.
	e2, _ := r.Clone().Get(listToolsName)
	out2, _ := e2.Execute(context.Background(), json.RawMessage(`{}`))
	if out2.Content != want {
		t.Fatalf("cloned list_tools output:\n%s", out2.Content)
	}
	// The pane's own list_tools is untouched (unprofiled panes unchanged).
	pe, _ := pane.Get(listToolsName)
	full, _ := pe.Execute(context.Background(), json.RawMessage(`{}`))
	if !strings.Contains(full.Content, "- bash:") {
		t.Fatalf("pane list_tools changed: %s", full.Content)
	}
}
