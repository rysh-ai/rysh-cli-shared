// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// e20 C-1 (design 093 H1 round 2): the orchestrator intercepts sub_agent and
// grounding_report by NAME. Before the fix it did so before the registry
// lookup, so a profiled pane that was never offered them could still call
// them: sub_agent spawned a child LLM run with a model-chosen system prompt and
// no ProfilePreamble, grounding_report{understood:false} paused the run with a
// question. Now an intercept runs only when the turn's registry holds the
// name; otherwise the call gets the ordinary "unknown tool" result.

// scriptedProv answers its first call with one tool call, every later call
// with end_turn, and records every call.
type scriptedProv struct {
	first provider.ToolCallRequest

	mu    sync.Mutex
	calls []scriptedCall
}

type scriptedCall struct {
	tools  []string
	system string
	conv   []provider.ConversationTurn
}

func (s *scriptedProv) Name() string                                     { return "scripted" }
func (s *scriptedProv) Complete(context.Context, string) (string, error) { return "", nil }
func (s *scriptedProv) CompleteWithTools(_ context.Context, conv []provider.ConversationTurn, specs []provider.ToolSpec, system string) (*provider.AgenticResponse, error) {
	names := make([]string, 0, len(specs))
	for _, sp := range specs {
		names = append(names, sp.Name)
	}
	s.mu.Lock()
	n := len(s.calls)
	s.calls = append(s.calls, scriptedCall{names, system, append([]provider.ConversationTurn(nil), conv...)})
	s.mu.Unlock()
	if n == 0 {
		return &provider.AgenticResponse{ToolCalls: []provider.ToolCallRequest{s.first}, StopReason: provider.StopReasonToolUse}, nil
	}
	return &provider.AgenticResponse{TextBlocks: []provider.TextBlock{{Text: "done"}}, StopReason: provider.StopReasonEndTurn}, nil
}

func (s *scriptedProv) snapshot() []scriptedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scriptedCall(nil), s.calls...)
}

// runProfiledTurn runs one prompt on a real profiled pane actor (allow-list
// [family_reply]; base registry holding bash, sub_agent and grounding_report)
// and returns the provider calls once the run has settled.
func runProfiledTurn(t *testing.T, first provider.ToolCallRequest) []scriptedCall {
	t.Helper()
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

	p := &scriptedProv{first: first}
	a := NewLLMPromptExecutionActor("pane-c1", 5, pub, nc, p,
		regOf("bash", SubAgentToolName, GroundingToolName), "BASE PROMPT", "", "", nil)
	a.SetOptInTools(regOf("family_reply"))
	a.SetAgentProfile(&msg.MsgSetPaneAgentProfile{SystemPrompt: "persona", AllowTools: []string{"family_reply"}})
	a.SetAutoApproveAll(true)

	sys := actor.NewActorSystem()
	pid := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return a }))
	defer sys.Root.Stop(pid)
	for deadline := time.Now().Add(5 * time.Second); ns.NumSubscriptions() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("actor never subscribed")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if err := pub.Send(msg.T("pane", "pane-c1", "llm_prompt_execution", "inbox"), &msg.MsgAgenticPrompt{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	// Settle: wait for the second (resume) call, then a quiet period so a
	// child run, had one been spawned, would have shown up.
	deadline := time.Now().Add(5 * time.Second)
	for len(p.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	return p.snapshot()
}

// toolResultFor returns the content of the tool result turn answering id.
func toolResultFor(conv []provider.ConversationTurn, id string) (string, bool) {
	for _, tn := range conv {
		if tn.Role == "tool" && tn.ToolCallID == id {
			return tn.Content, true
		}
	}
	return "", false
}

func assertOffListUnknown(t *testing.T, calls []scriptedCall, tool string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no provider call at all")
	}
	if got := strings.Join(calls[0].tools, ","); got != "family_reply" {
		t.Fatalf("first call offered %q, want only family_reply", got)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c.system, ProfilePreamble) {
			t.Errorf("call %d ran WITHOUT the profile preamble (system=%.40q): off-list %s spawned a run", i, c.system, tool)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("%d provider calls, want exactly 2 (the turn and its resume after the unknown-tool result): off-list %s was intercepted", len(calls), tool)
	}
	res, ok := toolResultFor(calls[1].conv, "t1")
	if !ok || !strings.Contains(res, "unknown tool: "+tool) {
		t.Fatalf("tool result for off-list %s = %q (found=%v), want \"unknown tool: %s\"", tool, res, ok, tool)
	}
}

func TestProfiledPane_OffListSubAgentIsUnknown(t *testing.T) {
	in, _ := json.Marshal(map[string]any{"task": "do x", "system_prompt": "CHILD-PROMPT-NO-PREAMBLE"})
	calls := runProfiledTurn(t, provider.ToolCallRequest{ID: "t1", Name: SubAgentToolName, Input: in})
	for i, c := range calls {
		if strings.Contains(c.system, "CHILD-PROMPT-NO-PREAMBLE") {
			t.Fatalf("BYPASS: call %d is a child run with the model-chosen system prompt (tools=%v)", i, c.tools)
		}
	}
	assertOffListUnknown(t, calls, SubAgentToolName)
}

func TestProfiledPane_OffListGroundingReportIsUnknown(t *testing.T) {
	in, _ := json.Marshal(map[string]any{"understood": false, "question": "where is the code?"})
	calls := runProfiledTurn(t, provider.ToolCallRequest{ID: "t1", Name: GroundingToolName, Input: in})
	assertOffListUnknown(t, calls, GroundingToolName)
}
