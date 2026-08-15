// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// Grounding: make the agent read the codebase the way a human does before it
// acts — locate the relevant code with search tools (glob / grep /
// symbol_search), read it, and iterate until the request maps to concrete
// files and symbols. If the codebase and documents don't yield the target,
// the agent asks the user WHERE the relevant code lives instead of guessing;
// if the prompt alone already makes clear that information is missing, it may
// ask immediately without ritually exhausting search first.
//
// Three modes (per LLM-execution actor, threaded into each orchestrator):
//
//	GroundingOff      — no protocol; legacy behavior.
//	GroundingPrompt   — ADVISORY: the grounding protocol is appended to the
//	                    system prompt, but nothing is enforced. Default for
//	                    sub-agents (pre-scoped by their parent) and
//	                    agents/humanoids (chat users don't want a grep
//	                    ritual forced on quick questions).
//	GroundingEnforced — the GATE: the run starts in PhaseGrounding where only
//	                    read-only exploration tools are allowed; mutating
//	                    tools stay locked until the model calls
//	                    grounding_report{understood: true}. Default for panes.
//
// grounding_report is an intercepted pseudo-tool (the sub_agent pattern):
// the registered executor is never run; the orchestrator handles the call.
//
//	understood=true            → exit PhaseGrounding, unlock all tools.
//	understood=false + question → surface the question to the user and PAUSE
//	                             the run (StoppedReasonAwaitingInfo); the
//	                             user's reply arrives as the next prompt and
//	                             resumes from the checkpoint.
const (
	GroundingOff      = "off"
	GroundingPrompt   = "prompt"
	GroundingEnforced = "enforced"
)

// GroundingToolName is the pseudo-tool the model calls to declare its
// understanding (or lack of it). Intercepted in executeTool before normal
// dispatch — the registered executor is a defensive no-op.
const GroundingToolName = "grounding_report"

// DefaultGroundingMaxIterations caps how many loop iterations a run may stay
// in PhaseGrounding before the gate opens anyway (fail-open: exploration
// budget exhausted must never brick the task).
const DefaultGroundingMaxIterations = 6

// GroundingReportParams mirrors the JSON schema of the grounding_report tool.
type GroundingReportParams struct {
	// Understood: true when the request now maps to concrete files/symbols
	// (or needs no codebase context at all).
	Understood bool `json:"understood"`
	// RelevantFiles lists the files the understanding is grounded on.
	RelevantFiles []string `json:"relevant_files,omitempty"`
	// Evidence is a one-paragraph summary of what was found and where.
	Evidence string `json:"evidence,omitempty"`
	// MissingInfo describes what could not be located (understood=false).
	MissingInfo string `json:"missing_info,omitempty"`
	// Question is the question to surface to the user (understood=false) —
	// typically "where does X live?".
	Question string `json:"question,omitempty"`
}

// ParseGroundingReport decodes a grounding_report tool-call payload.
func ParseGroundingReport(raw json.RawMessage) (*GroundingReportParams, error) {
	var p GroundingReportParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid grounding_report params: %w", err)
		}
	}
	return &p, nil
}

// ValidGroundingMode reports whether s is a recognised grounding mode.
func ValidGroundingMode(s string) bool {
	switch s {
	case GroundingOff, GroundingPrompt, GroundingEnforced:
		return true
	}
	return false
}

// LastGroundingReport scans the conversation backwards for the most recent
// grounding_report tool CALL (on an assistant turn) and returns its parsed
// projection for status display. The call's params — not the tool-result ack
// — carry the substance (files, evidence, question). Returns nil when the
// session has no grounded run yet.
func LastGroundingReport(turns []provider.ConversationTurn) *msg.GroundingReportInfo {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role != "assistant" {
			continue
		}
		for j := len(turns[i].ToolCalls) - 1; j >= 0; j-- {
			tc := turns[i].ToolCalls[j]
			if tc.Name != GroundingToolName {
				continue
			}
			p, err := ParseGroundingReport(tc.Input)
			if err != nil {
				continue // malformed call; keep scanning older turns
			}
			return &msg.GroundingReportInfo{
				Understood:    p.Understood,
				RelevantFiles: p.RelevantFiles,
				Evidence:      p.Evidence,
				MissingInfo:   p.MissingInfo,
				Question:      p.Question,
				TimestampMs:   turns[i].TimestampMs,
			}
		}
	}
	return nil
}

// GroundingAllowedTools is the read-only exploration set permitted while the
// grounding gate is closed. Everything else returns a permission error
// telling the model to ground first. bash is included because its own
// approval allowlist already gates mutating commands behind explicit user
// consent; ask_user is included so the model can ask mid-grounding. The set
// is a package variable so hosts can extend it (e.g. read-only MCP tools).
var GroundingAllowedTools = map[string]bool{
	"glob":            true,
	"grep":            true,
	"symbol_search":   true,
	"file_read":       true,
	"bash":            true,
	"list_tools":      true,
	"env_read":        true,
	"web_search":      true,
	"web_fetch":       true,
	"pane_inspect":    true,
	"session_history": true,
	"agents_list":     true,
	"ask_user":        true,
	"context":         true,
	"todo":            true,
	GroundingToolName: true,
}

// groundingAllowed reports whether a tool may run while grounding.
func groundingAllowed(toolName string) bool {
	return GroundingAllowedTools[toolName]
}

// GroundingReadOnlyBrowserActions is the subset of browser_action actions
// permitted while the grounding gate is closed: a web-mode pane must be able
// to OBSERVE the page (read text/elements, screenshot, inspect tabs) to
// ground itself, while mutating actions (click, type, navigate, execute_js…)
// stay locked until grounding_report. Package variable so hosts can extend.
var GroundingReadOnlyBrowserActions = map[string]bool{
	"get_text":     true,
	"get_html":     true,
	"get_elements": true,
	"get_value":    true,
	"get_tabs":     true,
	"screenshot":   true,
	"wait":         true,
}

// groundingAllowedCall is the param-aware gate check: browser_action is
// allowed while grounding only for read-only (observe) actions; every other
// tool is decided by name via GroundingAllowedTools.
func groundingAllowedCall(toolName string, params json.RawMessage) bool {
	if toolName == "browser_action" {
		var p struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return false
		}
		return GroundingReadOnlyBrowserActions[p.Action]
	}
	return groundingAllowed(toolName)
}

// DefaultGroundingPromptAdvisory is the grounding protocol appended to the
// system prompt in GroundingPrompt and GroundingEnforced modes. A VARIABLE
// (not const) so rysh-cli can replace it from an embedded prompt file, like
// DefaultSubAgentSystemPrompt.
var DefaultGroundingPromptAdvisory = `## Grounding protocol

Before answering questions about — or making changes to — a codebase, ground
yourself the way a human reader would:

1. LOCATE the relevant code: use glob / grep / symbol_search to find candidate
   files, then file_read what you find. Follow references until the request
   maps to concrete files and symbols.
2. ITERATE until you understand what is being asked in terms of the actual
   code. Base every claim on something you read; cite file paths.
3. If a handful of searches cannot locate the target, or the request is
   ambiguous no matter what the code says: DO NOT GUESS. Ask the user where
   the relevant code or documents live (which directory, file, or keyword).
4. If the prompt already gives you everything you need — or the question is
   not about this codebase — skip exploration and answer directly. You may
   also ask your question immediately when the prompt alone makes clear that
   information is missing; exhausting search first is not required.`

// DefaultGroundingPromptEnforced extends the advisory protocol with the gate
// contract. Also a VARIABLE for host override.
var DefaultGroundingPromptEnforced = DefaultGroundingPromptAdvisory + `

The grounding gate is ACTIVE for this task: only read-only exploration tools
are available until you call grounding_report. When the request maps to
concrete files (or plainly needs no codebase context), call
grounding_report{understood: true, relevant_files: [...], evidence: "..."} to
unlock all tools and proceed. If you cannot locate what the request refers
to, call grounding_report{understood: false, question: "..."} — the question
is shown to the user and the session pauses until they reply.`

// groundingBlockedMessage is the typed error content returned when a
// non-exploration tool is called while the gate is closed.
func groundingBlockedMessage(toolName string) string {
	if toolName == "browser_action" {
		return "mutating browser actions are locked during the grounding phase. OBSERVE the page first " +
			"(browser_action with get_text / get_elements / get_value / get_tabs / screenshot / wait), " +
			"then call grounding_report{understood: true} to unlock click/type/navigate — or " +
			"grounding_report{understood: false, question: ...} to ask the user and pause."
	}
	return fmt.Sprintf(
		"tool %q is locked during the grounding phase. Explore with read-only tools "+
			"(glob, grep, symbol_search, file_read, bash) until you understand the request, "+
			"then call grounding_report{understood: true} to unlock execution — or "+
			"grounding_report{understood: false, question: ...} to ask the user and pause.",
		toolName)
}

// groundedDigest renders the one-line step title for a successful grounding
// report.
func groundedDigest(p *GroundingReportParams) string {
	n := len(p.RelevantFiles)
	switch {
	case n == 0:
		return "grounded — no codebase context needed"
	case n == 1:
		return "grounded on " + p.RelevantFiles[0]
	default:
		return fmt.Sprintf("grounded on %d files (%s, …)", n, p.RelevantFiles[0])
	}
}

// questionDigest renders the step title for a surfaced question.
func questionDigest(q string) string {
	return "question: " + truncate(firstLine(strings.TrimSpace(q)), 80)
}

// handleGroundingReport services an intercepted grounding_report call.
//
//	understood=true  → open the gate (all tools unlock) and record what the
//	                   agent grounded on in the step stream + output.
//	understood=false → surface the question to the user and schedule an
//	                   awaiting-info pause (runLoop pauses after this tool
//	                   round; the user's reply resumes from the checkpoint).
//
// Works in every grounding mode: in prompt/off modes there is no gate to
// open, but the question path is still the pane-agnostic "ask the user and
// pause" mechanism (headless agents/humanoids have no interactive ask_user).
func (o *OrchestratorActor) handleGroundingReport(tc provider.ToolCallRequest) *tools.ToolOutput {
	p, err := ParseGroundingReport(tc.Input)
	if err != nil {
		o.emitOutput("error", err.Error()+"\n")
		return tools.ErrOutput(tools.ErrKindValidation, err.Error())
	}

	if p.Understood {
		digest := groundedDigest(p)
		o.emitToolCallHeader(toolCallMarker + " grounding_report(" + digest + ")")
		o.emitOutput("tool_result", toolResultBranch("✓ "+digest))
		o.emitStep(msg.StepGrounded, digest, p.Evidence, provider.TurnCategorySystem, "")
		if o.grounding {
			o.grounding = false
			return &tools.ToolOutput{
				Content: "Grounding accepted — all tools are now unlocked. Proceed with the task.",
				Metadata: map[string]string{
					"grounded_files": strings.Join(p.RelevantFiles, ","),
				},
			}
		}
		return &tools.ToolOutput{Content: "Grounding noted. Proceed with the task."}
	}

	q := strings.TrimSpace(p.Question)
	if q == "" {
		q = strings.TrimSpace(p.MissingInfo)
	}
	if q == "" {
		msgStr := "grounding_report with understood=false requires a `question` (or `missing_info`) describing what to ask the user"
		o.emitOutput("error", msgStr+"\n")
		return tools.ErrOutput(tools.ErrKindValidation, msgStr)
	}

	o.pendingQuestion = q
	o.emitToolCallHeader(toolCallMarker + " grounding_report(question for user)")
	o.emitOutput("text", "\n[question] "+q+"\n")
	o.emitStep(msg.StepQuestion, questionDigest(q), q, provider.TurnCategorySystem, "")
	return &tools.ToolOutput{
		Content: "Question surfaced to the user. The session will pause until they reply; " +
			"their answer will arrive as the next user message — do not repeat the question.",
	}
}
