// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// Step events are the low-bandwidth progress stream of an agentic run: one
// short, titled event per meaningful step (tool call, sub-agent, pause,
// completion). They are published on the per-execution steps subject
// (`rysh.pane.{id}.llm_prompt_execution.steps`) alongside the full output
// stream, so renderers can choose their fidelity:
//
//   - the pane terminal shows the full transcript (output subject),
//   - the Slack humanoid flow shows only step Titles (steps subject),
//     keeping channels readable while still signalling progress.

// stepTitle renders a short human-readable title for a tool call, e.g.
// "bash: go test ./..." or "edit: internal/tui/model.go". Falls back to the
// bare tool name when no concise label exists.
func stepTitle(toolName string, input json.RawMessage) string {
	label := toolCallLabel(toolName, input)
	if label == "" {
		return toolName
	}
	return toolName + ": " + truncate(firstLine(label), 80)
}

// subAgentStepTitle renders a short title for a sub-agent delegation from its
// task text, e.g. `sub-agent: audit the msg package`.
func subAgentStepTitle(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return "sub-agent"
	}
	return "sub-agent: " + truncate(firstLine(task), 80)
}

// subAgentResultDigest renders a one-line outcome summary for a finished
// sub-agent, used as the step-event detail and the conversation-turn Summary.
func subAgentResultDigest(done *msg.MsgOrchestratorDone) string {
	if done == nil {
		return "sub-agent returned no result"
	}
	status := "✓ done"
	if !done.Success {
		status = "✗ failed"
	}
	parts := []string{status}
	if n := len(done.FilesChanged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) changed", n))
	}
	if n := len(done.Errors); n > 0 {
		parts = append(parts, fmt.Sprintf("%d error(s)", n))
	}
	if s := strings.TrimSpace(done.Summary); s != "" {
		parts = append(parts, truncate(firstLine(s), 120))
	}
	return strings.Join(parts, " · ")
}

// emitStep publishes a structured MsgAgenticStep on the steps subject. It is
// best-effort: progress events must never block or fail the run.
func (o *OrchestratorActor) emitStep(kind, title, detail, category, origin string) {
	if o.stepsSubject == "" || o.pub == nil {
		return
	}
	_ = o.pub.Send(o.stepsSubject, &msg.MsgAgenticStep{
		OrchestratorID: o.id,
		Kind:           kind,
		Title:          title,
		Detail:         detail,
		Category:       category,
		Origin:         origin,
		Depth:          o.subAgentDepth,
		Iteration:      o.iteration,
		TimestampMs:    time.Now().UnixMilli(),
	})
}
