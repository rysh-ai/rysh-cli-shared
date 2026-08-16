// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// ---------------------------------------------------------------------------
// Agentic message tags
// ---------------------------------------------------------------------------

const (
	// Agentic control messages
	TagAgenticPrompt            = "MsgAgenticPrompt"
	TagAgenticCancel            = "MsgAgenticCancel"
	TagAgenticContinue          = "MsgAgenticContinue"
	TagAgenticStep              = "MsgAgenticStep"
	TagOrchestratorDone         = "MsgOrchestratorDone"
	TagToolCall                 = "MsgToolCall"
	TagToolResult               = "MsgToolResult"
	TagAgenticOutput            = "MsgAgenticOutput"
	TagAgenticStatus            = "MsgAgenticStatus"
	TagApprovalRequest          = "MsgApprovalRequest"
	TagApprovalResponse         = "MsgApprovalResponse"
	TagSpawnSubOrchestrator     = "MsgSpawnSubOrchestrator"
	TagSubOrchestratorResult    = "MsgSubOrchestratorResult"
	TagGetConversationHistory   = "MsgGetConversationHistory"
	TagConversationHistoryReply = "MsgConversationHistoryReply"
	TagRestoreConversation      = "MsgRestoreConversation"
	TagGetSessionMemory         = "MsgGetSessionMemory"
	TagSessionMemoryReply       = "MsgSessionMemoryReply"
	TagSessionMemoryReplace     = "MsgSessionMemoryReplace"
	TagSetGroundingMode         = "MsgSetGroundingMode"
	TagGetGroundingState        = "MsgGetGroundingState"
	TagGroundingStateReply      = "MsgGroundingStateReply"
	// Follow-up 2b: live prompt reload. The workspace command handler
	// (or the SIGHUP handler) re-runs ApplyPrompts on the Setup, then
	// broadcasts MsgReloadPrompts to every active LLM-execution / agent /
	// humanoid actor so they update their cached systemPrompt for the
	// NEXT prompt submission. In-flight orchestrators keep their captured
	// snapshot — by design.
	TagReloadPrompts = "MsgReloadPrompts"
	// TagSetRunBudget arms/clears a pane's auto-continue run budget (##web auto).
	TagSetRunBudget = "MsgSetRunBudget"
	// TagGetRunStatus / TagRunStatusReply query the live run/budget accounting
	// of an LLM execution actor (##auto <kind> runs). Request/reply.
	TagGetRunStatus   = "MsgGetRunStatus"
	TagRunStatusReply = "MsgRunStatusReply"
)

// ---------------------------------------------------------------------------
// Agentic Control Messages
// ---------------------------------------------------------------------------

// MsgAgenticPrompt is sent to LLMPromptExecutionActor when user submits a prompt.
//
// ContentBlocks is the multimodal carrier (follow-up 1b). When non-empty the
// LLM actor builds a structured ConversationTurn with both Prompt (as text)
// and the blocks (typically a single image). Pre-existing callers that only
// set Prompt continue to use the legacy text-only path.
type MsgAgenticPrompt struct {
	RequestID     string                  `json:"request_id"`
	Prompt        string                  `json:"prompt"`
	ContentBlocks []provider.ContentBlock `json:"content_blocks,omitempty"`
	// ScopeHint identifies the execution point (e.g. the invoking pane's scope
	// chain) so an agent/humanoid resolves tools against that pane's scope. Opaque
	// to this package; interpreted by the host's scope resolver. Empty for panes
	// (their registry is fixed) and for agents invoked outside any pane.
	ScopeHint string `json:"scope_hint,omitempty"`
}

// MsgAgenticCancel interrupts any in-flight orchestrator. The run is PAUSED,
// not discarded: the conversation (including synthetic results for in-flight
// tool calls) is preserved in session memory, so a follow-up
// MsgAgenticContinue — or the user typing "continue" — resumes from exactly
// where the run stopped. Triggered by double-Ctrl+C in the TUI and by
// `@@name stop` for agents/humanoids.
type MsgAgenticCancel struct{}

// MsgAgenticContinue resumes a paused agentic run (paused via cancel,
// iteration cap, or wall-clock cap) from the preserved conversation state.
// Equivalent to the user typing "continue" as a prompt while paused.
type MsgAgenticContinue struct{}

// MsgSetRunBudget arms (or clears) the auto-continue run budget on a pane's
// LLMPromptExecutionActor. When AutoContinue is true, a run that pauses on the
// per-leg iteration or wall-clock cap is resumed automatically (a fresh leg,
// with the previous leg's work already merged into the session) until the task
// finishes on its own, the leg allowance derived from MaxTotalIterations is
// spent, or MaxDurationMs elapses — then it pauses for a manual `continue`. A
// user cancel (double-Ctrl+C / @@stop) disarms it. Sent by `##web auto run`
// from the recipe's frontmatter (max_iterations / max_duration / auto_continue).
// AutoContinue=false clears any armed budget.
type MsgSetRunBudget struct {
	AutoContinue bool `json:"auto_continue"`
	// AutoApprove, when true, runs this run's tool calls without an approval
	// prompt (##web auto step.auto_approve). Independent of AutoContinue.
	AutoApprove bool `json:"auto_approve"`
	// AutoApprovePersist promotes AutoApprove from per-run state to the
	// actor's own default: it survives disarmRunBudget (which fires on every
	// terminal outcome, a clean finish included), so every LATER prompt to
	// this pane's agent runs approval-free too — not just the run being armed.
	// This is the fleet arm: a fleet agent launched approval-free used to be
	// approval-free for exactly one turn, and its second work order stalled at
	// the first `bash` on an approval prompt nobody was watching (E-45,
	// f8824f5's defect in native dress). Send AutoApprovePersist=true with
	// AutoApprove=false to turn the sticky grant back off. Policy gate rules
	// (always_gate / bash.deny) still override either form downstream.
	AutoApprovePersist bool  `json:"auto_approve_persist,omitempty"`
	MaxTotalIterations int   `json:"max_total_iterations,omitempty"`
	MaxDurationMs      int64 `json:"max_duration_ms,omitempty"`
	// StepInterval is the per-leg step cap (how many tool-iterations run before a
	// checkpoint/auto-resume). 0 → the actor's default. The auto-continue leg
	// count is ceil(MaxTotalIterations / StepInterval).
	StepInterval int `json:"step_interval,omitempty"`
	// MaxContextTokens caps the run's CUMULATIVE token usage — the sum of
	// input+output tokens across every LLM call (total "reading"), accumulated
	// across legs. When it's reached the run stops with StoppedReasonMaxTokens and
	// — unlike a step cap — does NOT auto-resume; it routes straight to the
	// finalizer. 0 → no token cap.
	MaxContextTokens int `json:"max_context_tokens,omitempty"`
	// FinalizerPrompt, when set, runs ONCE as a dedicated closing leg if the run
	// exhausts its budget (hit the iteration/time/token cap without finishing) — a
	// graceful wrap-up (e.g. "save what you've collected so far and summarize
	// what's incomplete"). It shares the run's conversation and is skipped when the
	// task finishes on its own.
	FinalizerPrompt string `json:"finalizer_prompt,omitempty"`
	// Finalizer sub-budget (the reserved budget_percent share): separate fresh
	// allowances for the closing leg — steps, duration, and cumulative tokens.
	// Token accounting resets when the finalizer takes over, so its share is a
	// clean fresh budget (not headroom). 0 → fall back to the built-in allowance.
	FinalizerMaxIterations    int   `json:"finalizer_max_iterations,omitempty"`
	FinalizerMaxDurationMs    int64 `json:"finalizer_max_duration_ms,omitempty"`
	FinalizerMaxContextTokens int   `json:"finalizer_max_context_tokens,omitempty"`
	// Model / Effort override the provider's model and effort for this run's
	// main legs (empty → the provider's configured defaults). Effort is the
	// Anthropic output_config.effort value (low|medium|high|xhigh|max).
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// FinalizerModel / FinalizerEffort override the model/effort for the
	// finalizer (takeover) leg only — the mechanical wrap-up can run on a
	// cheaper model. Empty → fall back to Model/Effort, then the provider's
	// defaults.
	FinalizerModel  string `json:"finalizer_model,omitempty"`
	FinalizerEffort string `json:"finalizer_effort,omitempty"`
	// OutputDir is the absolute directory the run's result files belong in.
	// It is restated in every synthetic continue/finalizer turn so the save
	// location survives context compaction on long runs (the original prompt —
	// the only other place it appears — is exactly what compaction drops).
	OutputDir string `json:"output_dir,omitempty"`
}

// MsgGetRunStatus asks an LLM execution actor for a snapshot of its live
// run/budget accounting (`##auto <kind> runs`). Request/reply.
type MsgGetRunStatus struct{}

// MsgRunStatusReply is the live run accounting behind `##auto <kind> runs`.
// Armed=false + InFlight=false means nothing is executing on this actor —
// the caller can drop the run from its registry.
type MsgRunStatusReply struct {
	PaneID   string `json:"pane_id"`
	Armed    bool   `json:"armed"`     // auto-continue budget armed
	InFlight bool   `json:"in_flight"` // an orchestrator leg is executing right now
	// Finalizer marks the takeover (wrap-up) leg; Paused/PausedReason mirror
	// the session pause state (max_iterations / awaiting_user_info / …).
	Finalizer    bool   `json:"finalizer,omitempty"`
	Paused       bool   `json:"paused,omitempty"`
	PausedReason string `json:"paused_reason,omitempty"`
	// TokensUsed is the run's cumulative token usage across COMPLETED legs
	// (the in-flight leg is added when it ends); MaxContextTokens is the armed
	// cumulative cap (0 → no token cap).
	TokensUsed       int `json:"tokens_used"`
	MaxContextTokens int `json:"max_context_tokens,omitempty"`
	// StepInterval is the per-leg step cap; ContinuesLeft the remaining
	// automatic resumes of the armed budget.
	StepInterval  int `json:"step_interval,omitempty"`
	ContinuesLeft int `json:"continues_left,omitempty"`
	// ArmedAtMs is when the current budget was armed (unix ms, 0 → not armed);
	// DeadlineMs the run's wall-clock deadline (0 → no time cap).
	ArmedAtMs  int64 `json:"armed_at_ms,omitempty"`
	DeadlineMs int64 `json:"deadline_ms,omitempty"`
	// Prompt-cache accounting across COMPLETED legs (in-flight leg added when
	// it ends). CacheReadTokens were served from Anthropic's prompt cache
	// (~10% price, excluded from TokensUsed); CacheWriteTokens were written to
	// it (1.25x price, counted); FreshInputTokens billed at the full rate.
	// hit% = read / (read + write + fresh).
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	FreshInputTokens int `json:"fresh_input_tokens,omitempty"`
	// TokensUsedTotal is the run's whole-life token count (survives finalizer
	// takeover and disarm; resets only on arm) — the number the run report
	// should print. TokensUsed remains the live budget-enforcement counter.
	TokensUsedTotal int `json:"tokens_used_total,omitempty"`
}

// StoppedReason values reported in MsgOrchestratorDone / MsgAgenticStep when
// a run ends without completing naturally.
const (
	StoppedReasonCancelled     = "cancelled"          // interrupted by user (double-Ctrl+C / @@stop)
	StoppedReasonMaxIterations = "max_iterations"     // paused at the iteration cap
	StoppedReasonMaxDuration   = "max_duration"       // paused at the wall-clock cap
	StoppedReasonMaxTokens     = "max_tokens"         // paused at the cumulative token budget
	StoppedReasonAwaitingInfo  = "awaiting_user_info" // paused on a question to the user (grounding/ask timeout); their reply resumes
)

// MsgOrchestratorDone is sent from OrchestratorActor back to LLMPromptExecutionActor
// when the task loop completes (success or failure).
// Conversation carries the full accumulated conversation (including tool calls and
// results) so the parent can merge it and subsequent orchestrators see the context.
type MsgOrchestratorDone struct {
	OrchestratorID string                      `json:"orchestrator_id"`
	Success        bool                        `json:"success"`
	Summary        string                      `json:"summary"`
	FilesChanged   []string                    `json:"files_changed,omitempty"`
	Errors         []string                    `json:"errors,omitempty"`
	Conversation   []provider.ConversationTurn `json:"-"` // in-process only, not serialized
	// Interrupted is true when the run stopped without finishing the task —
	// user cancel, iteration cap, or wall-clock cap. The parent marks its
	// session memory paused so the run can be resumed with "continue".
	Interrupted bool `json:"interrupted,omitempty"`
	// StoppedReason is one of the StoppedReason* constants when Interrupted.
	StoppedReason string `json:"stopped_reason,omitempty"`
	// TokensUsed is this leg's cumulative token usage (sum of input+output across
	// its LLM calls). The parent accumulates it across auto-continued legs to
	// enforce the run's cumulative token budget.
	TokensUsed int `json:"tokens_used,omitempty"`
	// Prompt-cache accounting for this leg (see MsgRunStatusReply for the
	// field semantics). The parent accumulates these across legs.
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	FreshInputTokens int `json:"fresh_input_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Step events (progress stream)
// ---------------------------------------------------------------------------

// Step event kinds published on the per-execution steps subject.
const (
	StepRunStart      = "run_start"      // orchestrator run began
	StepToolStart     = "tool_start"     // a tool call is about to execute
	StepToolEnd       = "tool_end"       // a tool call finished (Detail = result digest)
	StepSubAgentStart = "subagent_start" // a sub-agent was spawned
	StepSubAgentEnd   = "subagent_end"   // a sub-agent finished (Detail = summary)
	StepApprovalWait  = "approval_wait"  // waiting for user approval
	StepCompaction    = "compaction"     // context compaction ran
	StepPaused        = "paused"         // run paused (cancel / caps); resumable
	StepResumed       = "resumed"        // paused run resumed
	StepDone          = "done"           // run completed
	StepError         = "error"          // run failed
	StepFinalAnswer   = "final_answer"   // final assistant answer (Detail = full text)
	StepGrounded      = "grounded"       // grounding gate opened (Detail = evidence)
	StepQuestion      = "question"       // agent surfaced a question to the user (Detail = full question)
)

// MsgAgenticStep is a structured progress event describing one step of an
// agentic run. It is published to the per-execution steps subject
// (`…llm_prompt_execution.steps`) alongside — not instead of — the full
// output stream. Low-bandwidth renderers (the Slack humanoid flow, status
// UIs) subscribe here and show only Title, giving users a fluent progress
// feed without the full transcript.
type MsgAgenticStep struct {
	OrchestratorID string `json:"orchestrator_id"`
	Kind           string `json:"kind"`  // Step* constants above
	Title          string `json:"title"` // short human-readable step title
	// Detail optionally carries the larger payload (tool result digest,
	// sub-agent summary, final answer text). Renderers that only want titles
	// ignore it.
	Detail string `json:"detail,omitempty"`
	// Category / Origin mirror the conversation-turn categorisation
	// (provider.TurnCategory*): "tool" with the tool name, "subagent" with the
	// child orchestrator id, "ai" for model-level events.
	Category string `json:"category,omitempty"`
	Origin   string `json:"origin,omitempty"`
	// Depth is the sub-agent depth of the emitting orchestrator (0 = top).
	Depth       int   `json:"depth"`
	Iteration   int   `json:"iteration,omitempty"`
	TimestampMs int64 `json:"timestamp_ms"`
}

// MsgToolCall is sent from OrchestratorActor to execute a tool.
type MsgToolCall struct {
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
}

// MsgToolResult carries the result of a tool execution back to the orchestrator.
type MsgToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Content    string `json:"content"`
	Error      string `json:"error,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Approved   bool   `json:"approved"`
}

// ---------------------------------------------------------------------------
// Progress / Output Messages
// ---------------------------------------------------------------------------

// MsgAgenticOutput streams output from the orchestrator to the client.
type MsgAgenticOutput struct {
	OrchestratorID string            `json:"orchestrator_id"`
	Type           string            `json:"type"` // "thinking", "text", "tool_call", "tool_result", "diff", "error"
	Content        string            `json:"content"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// MsgAgenticStatus updates the agentic phase displayed in the status bar.
type MsgAgenticStatus struct {
	OrchestratorID string   `json:"orchestrator_id"`
	Phase          string   `json:"phase"` // "planning", "executing", "waiting_approval", "done", "error"
	Iteration      int      `json:"iteration"`
	MaxIterations  int      `json:"max_iterations"`
	ActiveTools    []string `json:"active_tools,omitempty"`
}

// ---------------------------------------------------------------------------
// Approval Messages
// ---------------------------------------------------------------------------

// ApprovalType discriminates the kind of approval being requested.
type ApprovalType string

const (
	ApprovalTypeDiff        ApprovalType = "diff"
	ApprovalTypeDestructive ApprovalType = "destructive_action"
	ApprovalTypeChoice      ApprovalType = "choice"
	ApprovalTypeQuestion    ApprovalType = "question"
)

// ApprovalDecision is the user's response to an approval request.
type ApprovalDecision string

const (
	DecisionYes               ApprovalDecision = "yes"
	DecisionYesAlways         ApprovalDecision = "yes_always"
	DecisionNo                ApprovalDecision = "no"
	DecisionNoWithExplanation ApprovalDecision = "no_with_explanation"
	DecisionChoiceSelected    ApprovalDecision = "choice_selected"
)

// Choice represents one option in a multi-choice approval.
type Choice struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// DiffPayload carries diff information for file-change approvals.
type DiffPayload struct {
	FilePath    string `json:"file_path"`
	UnifiedDiff string `json:"unified_diff"`
}

// MsgApprovalRequest is sent from the orchestrator to request user approval.
type MsgApprovalRequest struct {
	RequestID      string       `json:"request_id"`
	OrchestratorID string       `json:"orchestrator_id"`
	ToolCallID     string       `json:"tool_call_id"`
	Type           ApprovalType `json:"type"`
	Description    string       `json:"description"`
	Diff           *DiffPayload `json:"diff,omitempty"`
	Choices        []Choice     `json:"choices,omitempty"`
}

// MsgApprovalResponse carries the user's decision back to the orchestrator.
type MsgApprovalResponse struct {
	RequestID        string           `json:"request_id"`
	Decision         ApprovalDecision `json:"decision"`
	Reason           string           `json:"reason,omitempty"`
	ChoiceIdx        int              `json:"choice_idx,omitempty"`
	RespondingPaneID string           `json:"responding_pane_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Sub-Orchestrator Messages
// ---------------------------------------------------------------------------

// MsgSpawnSubOrchestrator requests the creation of a parallel sub-task.
type MsgSpawnSubOrchestrator struct {
	ParentOrchestratorID string `json:"parent_orchestrator_id"`
	SubOrchestratorID    string `json:"sub_orchestrator_id"`
	Task                 string `json:"task"`
	Context              string `json:"context,omitempty"`
}

// MsgSubOrchestratorResult reports the result of a sub-orchestrator.
type MsgSubOrchestratorResult struct {
	SubOrchestratorID    string `json:"sub_orchestrator_id"`
	ParentOrchestratorID string `json:"parent_orchestrator_id"`
	Success              bool   `json:"success"`
	Summary              string `json:"summary"`
}

// ---------------------------------------------------------------------------
// Conversation History
// ---------------------------------------------------------------------------

// MsgGetConversationHistory requests the conversation transcript from an LLMPromptExecutionActor.
type MsgGetConversationHistory struct {
	LastN int `json:"last_n"` // number of recent turns to return
}

// ConversationTurnInfo is a serializable conversation turn for the history response.
type ConversationTurnInfo struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ToolCallInfo `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
	// Category / Origin / Summary mirror provider.ConversationTurn metadata:
	// the pane-mode categorisation (shell/ai/rysh/chat) extended with agentic
	// origins (tool/subagent), the producer id, and the short step title.
	Category    string `json:"category,omitempty"`
	Origin      string `json:"origin,omitempty"`
	Summary     string `json:"summary,omitempty"`
	TimestampMs int64  `json:"timestamp_ms,omitempty"`
}

// ToolCallInfo is a serializable tool call for the history response.
type ToolCallInfo struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// MsgConversationHistoryReply carries the conversation transcript.
type MsgConversationHistoryReply struct {
	PaneID string                 `json:"pane_id"`
	Turns  []ConversationTurnInfo `json:"turns"`
}

// ---------------------------------------------------------------------------
// Conversation Restore (persistence across daemon restarts)
// ---------------------------------------------------------------------------

// MsgRestoreConversation restores a previously persisted conversation into an
// LLMPromptExecutionActor. Sent by PaneActor on startup when saved conversation
// data is found in the KV store.
type MsgRestoreConversation struct {
	Conversation []provider.ConversationTurn `json:"conversation"`
}

// ---------------------------------------------------------------------------
// Session memory fork (##hop): full-fidelity export + atomic replace
// ---------------------------------------------------------------------------

// MsgGetSessionMemory requests a FULL-FIDELITY export of an
// LLMPromptExecutionActor's session memory via request/reply. Unlike
// MsgGetConversationHistory (a lossy, capped projection for display and the
// session_history tool), the reply carries the raw provider.ConversationTurn
// slice — tool_use/tool_result pairs, content blocks, thinking blocks,
// categories — plus the pause checkpoint, so the session can be forked into
// another pane without losing replay fidelity.
type MsgGetSessionMemory struct{}

// MsgSessionMemoryReply is the full-fidelity session memory export.
type MsgSessionMemoryReply struct {
	PaneID       string                      `json:"pane_id"`
	Turns        []provider.ConversationTurn `json:"turns"`
	Paused       bool                        `json:"paused,omitempty"`
	PausedReason string                      `json:"paused_reason,omitempty"`
}

// MsgSessionMemoryReplace atomically REPLACES the receiving actor's session
// memory with the carried state (##hop fork, replace semantics). Any
// in-flight orchestrator on the receiver is interrupted first — its late
// completion is dropped by the stale-orchestrator guard — so the replaced
// memory cannot be clobbered by a run that started against the old state.
// The result is a fork: both panes hold identical conversation state at
// replace time and diverge independently afterwards.
type MsgSessionMemoryReplace struct {
	Turns        []provider.ConversationTurn `json:"turns"`
	Paused       bool                        `json:"paused,omitempty"`
	PausedReason string                      `json:"paused_reason,omitempty"`
	// Provenance, for logging and pane output.
	SourcePaneID string `json:"source_pane_id,omitempty"`
	SourceAlias  string `json:"source_alias,omitempty"`
}

// ---------------------------------------------------------------------------
// Grounding runtime control (##grounding command)
// ---------------------------------------------------------------------------

// MsgSetGroundingMode overrides an LLM-execution actor's grounding mode at
// runtime ("off" | "prompt" | "enforced"), or clears the override with ""
// (##grounding reset → revert to the host default). Overrides are persisted
// with the session memory, so they survive daemon restarts. The change
// applies from the NEXT prompt — an in-flight orchestrator keeps the mode it
// captured at spawn.
type MsgSetGroundingMode struct {
	Mode string `json:"mode"`
}

// MsgGetGroundingState requests the actor's grounding state via
// request/reply (##grounding status / report).
type MsgGetGroundingState struct{}

// GroundingReportInfo is the serializable projection of the most recent
// grounding_report tool call found in the session memory.
type GroundingReportInfo struct {
	Understood    bool     `json:"understood"`
	RelevantFiles []string `json:"relevant_files,omitempty"`
	Evidence      string   `json:"evidence,omitempty"`
	MissingInfo   string   `json:"missing_info,omitempty"`
	Question      string   `json:"question,omitempty"`
	TimestampMs   int64    `json:"timestamp_ms,omitempty"`
}

// MsgGroundingStateReply carries the grounding state for display.
type MsgGroundingStateReply struct {
	PaneID string `json:"pane_id"`
	// Mode is the effective grounding mode for the NEXT run.
	Mode string `json:"mode"`
	// DefaultMode is what the host originally configured (pane/agent default
	// or config override); Overridden is true when a runtime ##grounding
	// command changed Mode away from it.
	DefaultMode string `json:"default_mode"`
	Overridden  bool   `json:"overridden,omitempty"`
	// RunActive reports whether an orchestrator run is currently in flight.
	RunActive bool `json:"run_active,omitempty"`
	// LastReport is the most recent grounding_report call recorded in the
	// session memory (nil when no grounded run happened yet).
	LastReport *GroundingReportInfo `json:"last_report,omitempty"`
}

// ---------------------------------------------------------------------------
// Approval Flow Among Panes
// ---------------------------------------------------------------------------

const (
	TagCreateApprovalPane        = "MsgCreateApprovalPane"
	TagDestroyApprovalPane       = "MsgDestroyApprovalPane"
	TagPaneSetApprovalPaneGroups = "MsgPaneSetApprovalPaneGroups"
)

// MsgCreateApprovalPane is sent from OrchestratorActor to each target PaneGroupActor
// to spawn an ephemeral approval pane.
type MsgCreateApprovalPane struct {
	RequestID       string              `json:"request_id"`
	SourcePaneID    string              `json:"source_pane_id"`
	SourcePaneName  string              `json:"source_pane_name"`
	OrchestratorID  string              `json:"orchestrator_id"`
	ApprovalRequest *MsgApprovalRequest `json:"approval_request"`
	ResponseSubject string              `json:"response_subject"`
}

// MsgDestroyApprovalPane is sent from OrchestratorActor to each target PaneGroupActor
// after an approval is resolved (response received, timeout, or cancellation).
type MsgDestroyApprovalPane struct {
	RequestID string `json:"request_id"`
}

// MsgPaneSetApprovalPaneGroups configures which pane groups receive ephemeral
// approval panes for this pane's orchestrator approvals.
type MsgPaneSetApprovalPaneGroups struct {
	PaneGroupIDs []string `json:"pane_group_ids"`
	PaneName     string   `json:"pane_name,omitempty"` // source pane name shown in approval pane title
}

// ---------------------------------------------------------------------------
// Chat Output Routing (for autonomous agents)
// ---------------------------------------------------------------------------

const TagSetChatOutputPane = "MsgSetChatOutputPane"

// MsgSetChatOutputPane updates the chat output target pane for an LLMPromptExecutionActor.
// When PaneID is non-empty, LLMPromptExecutionActor output goes to that pane's chat buffer
// instead of the default AI output. When empty, routing reverts to normal.
type MsgSetChatOutputPane struct {
	PaneID string `json:"pane_id"`
}

// ---------------------------------------------------------------------------
// Memory state injection
// ---------------------------------------------------------------------------

const TagMemoryStateUpdate = "MsgMemoryStateUpdate"

// MsgMemoryStateUpdate delivers the current memory state to an LLMPromptExecutionActor.
// Sent by the MemoryManager whenever memory changes (new entry created, entries merged).
// The actor uses this to prepend memory context to the system prompt.
type MsgMemoryStateUpdate struct {
	State *MemoryState `json:"state"`
}

// ---------------------------------------------------------------------------
// Prompt hot-reload (Follow-up 2b)
// ---------------------------------------------------------------------------

// MsgReloadPrompts is broadcast to every active LLM-execution actor (per-pane,
// per-agent, per-humanoid) after the layered prompt store has been re-read.
// The actor swaps its cached `systemPrompt` so the NEXT prompt the user
// submits uses the new content. Any orchestrator currently mid-run keeps its
// captured snapshot — that's intentional: don't surprise an in-flight loop.
//
// EmailGovernance is only consumed by humanoid actors in email-governed mode.
// Other actors ignore it.
type MsgReloadPrompts struct {
	SystemPrompt    string `json:"system_prompt"`
	EmailGovernance string `json:"email_governance,omitempty"`
}
