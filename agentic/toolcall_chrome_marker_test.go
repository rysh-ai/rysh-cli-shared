// SPDX-License-Identifier: Apache-2.0

package agentic

// E-59 / R15: a chatbot visitor must see only the answer. The orchestrator is
// the last place that KNOWS whether an emit is answer text or tool-call chrome
// ("⏺ tool(…)" headers, "⎿ ✓ N lines" results, diffs, tool errors) — after
// SendConversation it is all just Content. So it marks chrome structurally on
// the ConversationMessage, and customer-facing consumers (rysh-server's widget
// proxy, transcript persistence, channel reply capture) drop it.
//
// The test drives the real emit helpers through a real NATSPublisher on an
// embedded NATS server and reads the raw wire JSON, so it pins the wire key.

import (
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

func TestChatOutputMarksToolCallChrome(t *testing.T) {
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

	type wire struct {
		content string
		chrome  bool
	}
	got := make(chan wire, 32)
	sub, err := nc.Subscribe("rysh.pane.pane-cb.output.chat", func(m *nats.Msg) {
		var env struct {
			P struct {
				Message map[string]any `json:"message"`
			} `json:"p"`
		}
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		c, _ := env.P.Message["content"].(string)
		chrome, _ := env.P.Message["tool_chrome"].(bool)
		got <- wire{c, chrome}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()

	o := &OrchestratorActor{
		id:               "orch-1",
		paneID:           "pane-cb",
		chatOutputPaneID: "pane-cb",
		pub:              msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
		lastOutputNL:     true,
	}

	emits := []struct {
		name   string
		emit   func()
		chrome bool
	}{
		{"streamed answer", func() { o.emitConversationOutput("Here are the pets: ", true) }, false},
		{"answer text", func() { o.emitOutput("text", "Rex and Bella.") }, false},
		{"tool-call header", func() { o.emitToolCallHeader(toolCallMarker + " petstore_findPetsByStatus(available)") }, true},
		{"tool result", func() {
			o.emitOutput("tool_result", toolResultBranch(toolResultSummary(&tools.ToolOutput{Content: "a\nb"})))
		}, true},
		{"tool error", func() { o.emitToolError("connection refused") }, true},
		{"diff", func() { o.emitOutput("diff", "+x\n") }, true},
	}
	for _, e := range emits {
		e.emit()
		_ = nc.Flush()
		select {
		case w := <-got:
			if w.chrome != e.chrome {
				t.Errorf("%s (%q): tool_chrome=%v, want %v", e.name, w.content, w.chrome, e.chrome)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: nothing published on output.chat", e.name)
		}
	}
}
