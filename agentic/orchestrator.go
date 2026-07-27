package agentic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/secretnat"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// OrchestratorPhase represents the current phase of the orchestrator loop.
type OrchestratorPhase string

const (
	PhasePlanning        OrchestratorPhase = "planning"
	PhaseExecuting       OrchestratorPhase = "executing"
	PhaseWaitingApproval OrchestratorPhase = "waiting_approval"
	PhaseEvaluating      OrchestratorPhase = "evaluating"
	PhaseDone            OrchestratorPhase = "done"
	PhaseError           OrchestratorPhase = "error"
	PhaseCompacting      OrchestratorPhase = "compacting"
	// PhasePaused: the run stopped without finishing (user interrupt via
	// double-Ctrl+C, iteration cap, or wall-clock cap) but its state is fully
	// preserved in session memory — "continue" resumes from this checkpoint.
	PhasePaused OrchestratorPhase = "paused"
	// PhaseGrounding: the run is reading the codebase like a human before
	// acting — only read-only exploration tools are allowed until the model
	// calls grounding_report{understood: true}. See grounding.go.
	PhaseGrounding OrchestratorPhase = "grounding"
)

// DefaultContextTokenLimit is the input-token budget used when the host does
// not specify one. Claude models accept ~200k input tokens; the limit leaves
// headroom for the response and tool output.
const DefaultContextTokenLimit = 160000

// compactionThreshold is the fraction of contextTokenLimit at which the oldest
// turns are summarized and dropped.
const compactionThreshold = 0.75

// compactionKeepRecentTurns is the number of most-recent conversation turns
// preserved verbatim during compaction.
const compactionKeepRecentTurns = 12

// Compaction pins the run's ORIGINAL task turn verbatim (capped at
// pinnedTaskMaxChars) inside the synthetic summary turn, so instructions that
// only appear in the first prompt — output paths, file-naming conventions,
// formats — survive any number of compactions instead of degrading into a
// lossy summary.
const (
	pinnedTaskMaxChars      = 4000
	pinnedTaskHeader        = "[Original task — kept verbatim through compaction]"
	compactionSummaryHeader = "[Summary of earlier conversation, compacted to save context]"
)

// OrchestratorActor runs the agentic loop for a single user request.
// It calls the LLM, dispatches tool calls, handles approvals, and loops
// until the task is complete or limits are reached.
type OrchestratorActor struct {
	id     string
	paneID string
	// agentName is set only when this orchestrator serves a named autonomous
	// agent or humanoid (design 003 "by agent" accounting). Empty for a regular
	// pane. It is stamped onto every MsgUsageRecord so the UsageActor can roll up
	// spend by agent; for a pane, paneID already covers it. It is deliberately
	// NOT derived from paneID: for agents paneID happens to equal the name, but a
	// pane's paneID is a UUID, so only an explicit value can tell them apart.
	agentName string
	pub       *msg.NATSPublisher
	nc        *nats.Conn
	prov      provider.AgenticProvider
	tools     *tools.ToolRegistry

	// Loop state
	phase         OrchestratorPhase
	iteration     int
	maxIterations int
	conversation  []provider.ConversationTurn
	systemPrompt  string
	ctx           context.Context // cancellable context from parent

	// Context-window management. contextTokenLimit is the effective input-token
	// budget for the conversation; when the last reported prompt size exceeds
	// compactionThreshold of it, the oldest turns are summarized and dropped.
	contextTokenLimit int
	lastInputTokens   int // TotalInputTokens from the most recent LLM response

	// Approval state
	autoApproved    map[string]bool
	pendingApproval *pendingApproval
	// autoApproveAll, when true, treats every tool call as pre-approved and
	// never publishes an approval request or spawns an approval pane. Used by
	// headless actors with no human at the keyboard (humanoids/agents).
	autoApproveAll bool

	// Loop detection
	callHistory   []loopEntry
	loopThreshold int // max repeats before blocking (default 3)

	// Results
	filesChanged []string
	errors       []string

	// Interrupt state. When the run stops without completing (user cancel,
	// iteration cap, wall-clock cap) interrupted is set with the matching
	// msg.StoppedReason* so the parent pauses — rather than discards — the
	// session (resumable via "continue").
	interrupted   bool
	stoppedReason string

	// NATS subjects for output
	outputSubject         string
	statusSubject         string
	approvalSubject       string
	stepsSubject          string
	pipelineOutputSubject string

	// Chat output routing (for autonomous agents).
	chatOutputPaneID string

	// Approval pane groups: when non-empty, approval requests create ephemeral
	// panes in these pane groups instead of publishing to the legacy approval subject.
	approvalPaneGroups []string

	// Source pane name for display in ephemeral approval panes.
	paneName string

	// Sub-agent state. subAgentDepth is 0 for a top-level orchestrator; each
	// spawned child increments it. pendingSubAgents routes incoming
	// MsgOrchestratorDone messages from child orchestrators back to the
	// goroutine that is blocked waiting on them (keyed by child orch ID).
	subAgentDepth    int
	pendingSubAgents map[string]chan *msg.MsgOrchestratorDone

	// readTracker remembers files the model has read during THIS orchestrator
	// run, so file_edit / multi_edit / file_write can refuse to operate on a
	// stale view of disk. Phase 3 L. Each orchestrator (including child
	// sub-agents) has its own fresh tracker.
	readTracker *ReadTracker

	// permissions optionally gates tool calls before normal dispatch with
	// allow / deny / ask rules per (tool, path). Nil = no policy (everything
	// falls through to the existing approval flow). Phase 3 N.
	permissions *PermissionPolicy

	// maxDuration is a top-level wall-clock cap on the agentic loop. 0 means
	// no cap (iteration cap is the only fail-safe). Phase 3 smaller-wins.
	maxDuration time.Duration
	startedAt   time.Time

	// maxContextTokens caps this leg's CUMULATIVE token usage (tokensUsed). When
	// reached, the loop pauses with StoppedReasonMaxTokens. 0 means no cap. Set via
	// SetMaxContextTokens (the parent passes the run's remaining token budget).
	maxContextTokens int
	// latestScreenshot is the CURRENT page image, injected as an EPHEMERAL
	// trailing user turn on each request (see requestConversation) and never
	// stored in o.conversation. Storing screenshots in the transcript and
	// pruning older ones ("latest wins") mutated the transcript mid-stream,
	// which invalidated Anthropic's incremental prompt cache every round and
	// burned the run's token budget near-quadratically (each round re-created
	// the whole conversation as CacheCreation tokens, counted at full weight).
	latestScreenshot *provider.ContentBlock

	// Prompt-cache accounting for this leg (surfaced in the status line and
	// MsgOrchestratorDone): reads are ~10%-price and excluded from the budget;
	// writes are 1.25x and counted; fresh input is full price.
	cacheReadTokens  int
	cacheWriteTokens int
	freshInputTokens int

	// tokensUsed accumulates input+output tokens across this leg's LLM calls. It is
	// reported in MsgOrchestratorDone so the parent can enforce a cumulative token
	// budget across auto-continued legs.
	tokensUsed int

	// metrics is the observability sink. Defaults to NoopMetricsSink{} so
	// the orchestrator never has to nil-check. Follow-up item 3.
	metrics MetricsSink

	// cwdResolver, when set, returns the working directory tools should run in
	// (the pane's live shell cwd). It is evaluated once at the start of runLoop
	// and applied to this run's cloned tool registry, so AI-mode tools follow
	// the shell after a `cd`. Nil → tools keep their construction-time workDir.
	cwdResolver func() string

	// lastOutputNL is true when the most recently emitted output ended on a
	// newline (i.e. the stream is at the start of a line). Used by
	// emitToolCallHeader to force tool-call headers onto their own line.
	lastOutputNL bool

	// Grounding state (see grounding.go). groundingMode selects off /
	// prompt / enforced; grounding is true while the enforced gate is
	// closed (read-only tools only); groundingIters counts loop iterations
	// spent grounding (fail-open at DefaultGroundingMaxIterations).
	// pendingQuestion, when set by grounding_report{understood:false} or an
	// ask_user timeout, makes the loop PAUSE with
	// StoppedReasonAwaitingInfo after the current tool round — the user's
	// reply arrives as the next prompt and resumes from the checkpoint.
	groundingMode   string
	grounding       bool
	groundingIters  int
	pendingQuestion string

	// nat is the SecretNAT (ReSet) session for this conversation. When set
	// and enabled, tool inputs are restored to real secret values just-in-time
	// for local execution (transient copies only) and tool outputs are
	// sanitized AT SOURCE — before display, metrics, step events, and the
	// tool_result turn — so real secrets never enter o.conversation /
	// SessionMemory / JetStream KV. Nil → all hooks are no-ops.
	nat secretnat.SessionHandle
}

type pendingApproval struct {
	requestID  string
	toolCallID string
	toolName   string
	params     json.RawMessage
	output     *tools.ToolOutput
}

// loopEntry tracks a recent tool call for loop detection.
type loopEntry struct {
	toolName   string
	paramsHash string
}

// detectLoop checks if the same tool+params combination has been called
// too many times recently. Returns true if a loop is detected.
func (o *OrchestratorActor) detectLoop(toolName string, params json.RawMessage) bool {
	threshold := o.loopThreshold
	if threshold <= 0 {
		threshold = 3
	}

	// Build a hash key from tool name + the FULL params. Hashing the whole
	// payload (instead of a 200-char prefix) avoids false positives on
	// legitimate repeated calls that only differ deep in the params — e.g.
	// three edits to the same file whose old_string diverges late.
	sum := sha256.Sum256(params)
	hash := toolName + ":" + hex.EncodeToString(sum[:])

	o.callHistory = append(o.callHistory, loopEntry{
		toolName:   toolName,
		paramsHash: hash,
	})

	// Keep only the last 20 entries.
	if len(o.callHistory) > 20 {
		o.callHistory = o.callHistory[len(o.callHistory)-20:]
	}

	// Count how many times this exact call appears in recent history.
	count := 0
	for _, entry := range o.callHistory {
		if entry.paramsHash == hash {
			count++
		}
	}

	return count >= threshold
}

// NewOrchestratorActor creates an orchestrator for a single task.
func NewOrchestratorActor(
	id string,
	paneID string,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	prov provider.AgenticProvider,
	toolRegistry *tools.ToolRegistry,
	conversation []provider.ConversationTurn,
	systemPrompt string,
	autoApproved map[string]bool,
	ctx context.Context,
	maxIterations int,
	pipelineOutputSubject string,
	chatOutputPaneID string,
	approvalPaneGroups []string,
	paneName string,
	contextTokenLimit int,
	subAgentDepth int,
	agentName string,
) *OrchestratorActor {
	// Copy conversation to avoid mutations to parent's slice
	convCopy := make([]provider.ConversationTurn, len(conversation))
	copy(convCopy, conversation)

	if contextTokenLimit <= 0 {
		contextTokenLimit = DefaultContextTokenLimit
	}

	// Copy auto-approved map
	approvedCopy := make(map[string]bool)
	for k, v := range autoApproved {
		approvedCopy[k] = v
	}

	// Clone the shared registry so per-pane tools can be added without
	// affecting the shared instance.
	paneRegistry := toolRegistry.Clone()

	return &OrchestratorActor{
		id:                    id,
		paneID:                paneID,
		agentName:             agentName,
		pub:                   pub,
		nc:                    nc,
		prov:                  prov,
		tools:                 paneRegistry,
		phase:                 PhasePlanning,
		iteration:             0,
		maxIterations:         maxIterations,
		conversation:          convCopy,
		systemPrompt:          systemPrompt,
		lastOutputNL:          true, // start-of-stream counts as a fresh line
		ctx:                   ctx,
		autoApproved:          approvedCopy,
		callHistory:           make([]loopEntry, 0),
		loopThreshold:         3,
		filesChanged:          make([]string, 0),
		errors:                make([]string, 0),
		outputSubject:         msg.T("pane", paneID, "llm_prompt_execution", "output"),
		statusSubject:         msg.T("pane", paneID, "llm_prompt_execution", "status"),
		approvalSubject:       msg.T("pane", paneID, "approval", "request"),
		stepsSubject:          msg.T("pane", paneID, "llm_prompt_execution", "steps"),
		pipelineOutputSubject: pipelineOutputSubject,
		chatOutputPaneID:      chatOutputPaneID,
		approvalPaneGroups:    approvalPaneGroups,
		paneName:              paneName,
		contextTokenLimit:     contextTokenLimit,
		subAgentDepth:         subAgentDepth,
		pendingSubAgents:      make(map[string]chan *msg.MsgOrchestratorDone),
		readTracker:           NewReadTracker(),
		metrics:               NoopMetricsSink{},
	}
}

// SetAutoApproveAll enables headless auto-approval: every tool call is treated
// as pre-approved, so no approval request is published and no approval pane is
// spawned. Used for humanoids/agents that have no human approver.
func (o *OrchestratorActor) SetAutoApproveAll(b bool) {
	o.autoApproveAll = b
}

// SetPermissionPolicy installs a fine-grained tool-permission policy.
// Pass nil to remove it. Phase 3 N.
func (o *OrchestratorActor) SetPermissionPolicy(p *PermissionPolicy) {
	o.permissions = p
}

// SetMaxDuration installs a wall-clock cap on the agentic loop. 0 disables
// the cap (iteration limit remains the only fail-safe). Phase 3 smaller-wins.
func (o *OrchestratorActor) SetMaxDuration(d time.Duration) {
	o.maxDuration = d
}

// SetMaxContextTokens installs a cumulative token cap: the loop pauses with
// StoppedReasonMaxTokens once this leg's total token usage (input+output) reaches
// n. The parent passes the run's remaining budget. 0 disables the cap.
func (o *OrchestratorActor) SetMaxContextTokens(n int) {
	o.maxContextTokens = n
}

// SetGroundingMode selects the grounding protocol for this run: GroundingOff,
// GroundingPrompt (advisory system-prompt section), or GroundingEnforced
// (read-only tool gate until grounding_report{understood:true}). See
// grounding.go.
func (o *OrchestratorActor) SetGroundingMode(mode string) {
	o.groundingMode = mode
}

// SetSecretNAT installs the SecretNAT session for this run. Pass nil to
// disable. The provider decorator (secretnat.Wrap) handles the HTTP
// boundary; these hooks handle the tool seam and KV hygiene.
func (o *OrchestratorActor) SetSecretNAT(s secretnat.SessionHandle) {
	o.nat = s
}

// natEnabled reports whether SecretNAT translation is active for this run.
func (o *OrchestratorActor) natEnabled() bool {
	return o.nat != nil && o.nat.Enabled()
}

// natRestoreInput returns tool input with synthetic tokens restored to real
// values for LOCAL execution only. The restored bytes are transient — they
// must never be written back to tc.Input or the conversation.
func (o *OrchestratorActor) natRestoreInput(in json.RawMessage) json.RawMessage {
	if !o.natEnabled() {
		return in
	}
	return o.nat.RestoreJSON(in)
}

// natDisplayText restores synthetic tokens for DISPLAY when the opt-in
// restore_display knob is on. The conversation copy always keeps tokens;
// enabling this writes real values into the pane output buffer (persisted to
// pane KV and forwarded to listeners/shares) — hence off by default.
func (o *OrchestratorActor) natDisplayText(text string) string {
	if o.nat == nil || !o.nat.Enabled() || !o.nat.RestoreDisplay() {
		return text
	}
	return o.nat.Restore(text)
}

// natSanitizeOutput sanitizes a tool's output at source so every consumer —
// display stream, metrics, step events, and the tool_result turn that lands
// in SessionMemory/KV — sees synthetic tokens, never real values. Screenshot
// metadata (base64 image bytes) is left untouched.
func (o *OrchestratorActor) natSanitizeOutput(out *tools.ToolOutput) *tools.ToolOutput {
	if out == nil || !o.natEnabled() {
		return out
	}
	out.Content = o.nat.Sanitize(out.Content)
	out.Error = o.nat.Sanitize(out.Error)
	return out
}

// Receive handles messages for the orchestrator.
func (o *OrchestratorActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		// Kick off the first iteration
		go o.runLoop(ctx)

	case *actor.Stopping:
		// Context cancellation handles goroutines

	case *msg.MsgApprovalResponse:
		o.handleApprovalResponse(ctx, m)

	case *msg.MsgToolResult:
		o.handleToolResult(ctx, m)

	case *subAgentSpawnReq:
		o.handleSubAgentSpawn(ctx, m)

	case *msg.MsgOrchestratorDone:
		// Routed here when a CHILD orchestrator (spawned via sub_agent) finishes
		// and sends to ctx.Parent() — which is this orchestrator. The top-level
		// orchestrator's own MsgOrchestratorDone goes to its parent
		// (LLMPromptExecutionActor) and never reaches this case.
		if ch, ok := o.pendingSubAgents[m.OrchestratorID]; ok {
			delete(o.pendingSubAgents, m.OrchestratorID)
			// Non-blocking: the channel is buffered (size 1); if the waiter has
			// already given up (timeout / parent cancel) the send still fits.
			select {
			case ch <- m:
			default:
			}
		}
	}
}

// runLoop is the main agentic loop. It runs in a goroutine and communicates
// with the actor via NATS messages (publishing output/status) and through
// the parent context for cancellation.
func (o *OrchestratorActor) runLoop(ctx actor.Context) {
	defer func() {
		// A paused/interrupted run may have stopped between an assistant
		// tool_use and its tool_result. Close any dangling calls with
		// synthetic "[error kind=cancelled]" results so the preserved
		// conversation is resumable (the Messages API rejects transcripts
		// with unanswered tool_use blocks).
		if o.interrupted {
			o.conversation = CloseDanglingToolCalls(o.conversation, "interrupted by user — the call did not run; re-issue it if still needed")
		}

		// Terminal step event for low-bandwidth renderers (Slack flow).
		switch {
		case o.interrupted:
			o.emitStep(msg.StepPaused, pausedStepTitle(o.stoppedReason), "", provider.TurnCategorySystem, o.stoppedReason)
		case o.phase == PhaseError:
			o.emitStep(msg.StepError, "run failed", strings.Join(o.errors, "; "), provider.TurnCategorySystem, "")
		default:
			if last := lastAssistantText(o.conversation); last != "" {
				o.emitStep(msg.StepFinalAnswer, "final answer", last, provider.TurnCategoryAI, "")
			}
			o.emitStep(msg.StepDone, "done", o.buildSummary(), provider.TurnCategorySystem, "")
		}

		// Flush the NATS connection buffer to guarantee all pending output
		// publishes reach the server before the terminal status. nc.Publish
		// buffers asynchronously, so without an explicit Flush the status
		// message can overtake the final text output even when published
		// later from the same goroutine.
		_ = o.pub.Flush()

		// Emit terminal status (done/paused/error) after all output is flushed.
		o.emitStatus(o.phase)

		// Report completion to parent LLMPromptExecutionActor.
		// Include the full conversation so the parent can merge it, preserving
		// tool call results (like draft_id) for subsequent orchestrator sessions.
		ctx.Send(ctx.Parent(), &msg.MsgOrchestratorDone{
			OrchestratorID:   o.id,
			Success:          o.phase == PhaseDone && len(o.errors) == 0,
			Summary:          o.buildSummary(),
			FilesChanged:     o.filesChanged,
			Errors:           o.errors,
			Conversation:     o.conversation,
			Interrupted:      o.interrupted,
			StoppedReason:    o.stoppedReason,
			TokensUsed:       o.tokensUsed,
			CacheReadTokens:  o.cacheReadTokens,
			CacheWriteTokens: o.cacheWriteTokens,
			FreshInputTokens: o.freshInputTokens,
		})
	}()

	if o.startedAt.IsZero() {
		o.startedAt = time.Now()
	}

	// Point working-directory-aware tools at the pane's live shell cwd so AI
	// mode follows the shell after a `cd`. Resolved once per run; a blank
	// result leaves tools at their construction-time workDir. Safe to mutate
	// here because o.tools is this run's private clone (cloneTooler deep-copies
	// the working-directory tools), so it can't leak into other panes/runs.
	if o.cwdResolver != nil {
		o.tools.SetWorkDir(o.cwdResolver())
	}

	o.emitStep(msg.StepRunStart, "run started", "", provider.TurnCategoryAI, "")

	// Grounding protocol: append the grounding section to this run's system
	// prompt, and in enforced mode close the gate (read-only tools only until
	// grounding_report{understood: true}). See grounding.go.
	switch o.groundingMode {
	case GroundingEnforced:
		o.grounding = true
		o.systemPrompt = o.systemPrompt + "\n\n" + DefaultGroundingPromptEnforced
	case GroundingPrompt:
		o.systemPrompt = o.systemPrompt + "\n\n" + DefaultGroundingPromptAdvisory
	}

	for {
		// Check cancellation: a user interrupt (double-Ctrl+C / @@stop) or a
		// replacement prompt cancelled the context. The run PAUSES — state is
		// preserved by the defer above and the parent's session memory — so
		// "continue" resumes from exactly here.
		if o.ctx.Err() != nil {
			o.markPaused(msg.StoppedReasonCancelled)
			return
		}

		// Check wall-clock cap (Phase 3 smaller-wins). Pauses instead of
		// erroring: long tasks can be resumed with a fresh budget.
		if o.maxDuration > 0 && time.Since(o.startedAt) >= o.maxDuration {
			o.emitOutput("text", fmt.Sprintf("\n[Paused: reached maximum duration (%s) — type 'continue' to resume]\n", o.maxDuration))
			o.markPaused(msg.StoppedReasonMaxDuration)
			return
		}

		// Check cumulative token budget. Like the time cap this is a hard ceiling,
		// not a leg boundary: the parent routes StoppedReasonMaxTokens straight to
		// the finalizer. Measures total tokens processed this leg (input+output);
		// the parent passes the run's REMAINING budget as the cap, so this enforces
		// the cumulative budget across auto-continued legs.
		if o.maxContextTokens > 0 && o.tokensUsed >= o.maxContextTokens {
			o.emitOutput("text", fmt.Sprintf("\n[Paused: reached token budget (%d tokens processed) — type 'continue' to resume]\n", o.maxContextTokens))
			o.markPaused(msg.StoppedReasonMaxTokens)
			return
		}

		// Check iteration limit. Pauses instead of erroring: the conversation
		// is preserved and "continue" resumes with a fresh iteration budget.
		if o.iteration >= o.maxIterations {
			o.emitOutput("text", fmt.Sprintf("\n[Paused: reached maximum iterations (%d) — type 'continue' to resume]\n", o.maxIterations))
			o.markPaused(msg.StoppedReasonMaxIterations)
			return
		}

		o.iteration++

		// Grounding budget: fail-open. Exploration must never brick the task,
		// so after DefaultGroundingMaxIterations loop iterations the gate
		// opens anyway; the model's next grounding_report (if any) just acks.
		if o.grounding {
			o.groundingIters++
			if o.groundingIters > DefaultGroundingMaxIterations {
				o.grounding = false
				o.emitOutput("text", "\n[grounding budget exhausted — proceeding with current understanding]\n")
				o.emitStep(msg.StepGrounded, "grounding budget exhausted — gate opened", "", provider.TurnCategorySystem, "")
			}
		}

		// Compact the conversation if the previous turn's prompt size is
		// approaching the context-window budget. This summarizes and drops the
		// oldest turns so long sessions don't overflow the context window.
		o.maybeCompact()

		if o.grounding {
			o.emitStatus(PhaseGrounding)
		} else {
			o.emitStatus(PhaseExecuting)
		}

		// Call LLM with conversation + tools
		toolSpecs := o.tools.AllSpecs()
		providerSpecs := make([]provider.ToolSpec, len(toolSpecs))
		for i, ts := range toolSpecs {
			providerSpecs[i] = provider.ToolSpec{
				Name:        ts.Name,
				Description: ts.Description,
				Parameters:  ts.Parameters,
			}
		}

		llmStart := time.Now()
		resp, streamed, err := o.callProvider(providerSpecs)
		// Follow-up item 3: record LLM call wall-clock + usage.
		if resp != nil && err == nil {
			o.metrics.RecordLLMCall(o.prov.Name(), time.Since(llmStart), resp.Usage)
		}
		if err != nil {
			if o.ctx.Err() != nil {
				// Cancelled mid-call: pause with state preserved.
				o.markPaused(msg.StoppedReasonCancelled)
				return
			}
			o.emitOutput("error", fmt.Sprintf("\n[LLM error: %v]\n", err))
			o.errors = append(o.errors, err.Error())
			// HTTP 400 invalid_request errors are deterministic: the same
			// conversation re-sent yields the same rejection, so looping here
			// silently burns the run's whole iteration/token budget on
			// identical failures (observed: 8+ media-type 400s in one run).
			// Fail the turn once with a clear message instead.
			if provider.IsInvalidRequestErr(err) {
				o.emitOutput("error", "[request rejected as invalid (HTTP 400) — not retrying; fix the request payload and resume]\n")
				o.phase = PhaseError
				return
			}
			// Retry on transient errors (the provider has already exhausted
			// its own backoff policy by the time the error reaches us).
			if o.iteration < o.maxIterations {
				time.Sleep(2 * time.Second)
				continue
			}
			o.phase = PhaseError
			return
		}

		// Record token usage for context budgeting and surface it to the UI.
		if total := resp.Usage.TotalInputTokens(); total > 0 {
			o.lastInputTokens = total
			o.emitContextStatus()
		}
		// Accumulate this call's NEW tokens (excluding re-read cache) for the run's
		// cumulative token budget — see Usage.NewTokens. Summing TotalInputTokens
		// here would re-count the whole growing conversation every call.
		o.tokensUsed += resp.Usage.NewTokens()
		// Prompt-cache accounting (surfaced in the status line + run status):
		// a healthy browser run should show a high hit% — a low one means the
		// stored prefix is being invalidated and the budget is burning at
		// full price.
		o.cacheReadTokens += resp.Usage.CacheReadInputTokens
		o.cacheWriteTokens += resp.Usage.CacheCreationInputTokens
		o.freshInputTokens += resp.Usage.InputTokens

		// Usage ledger (design 003): emit one record per LLM call to the pane's
		// usage subject where the UsageActor aggregates spend. Cost is priced by
		// the UsageActor, so the pricing table lives in exactly one place.
		o.emitUsage(resp.Usage)

		// Process text blocks. When the response came from the streaming
		// path the text was already emitted incrementally via the callback
		// above, so here we only accumulate it into the assistant turn that
		// gets appended to the conversation.
		var assistantContent strings.Builder
		for _, tb := range resp.TextBlocks {
			if tb.Text == "" {
				continue
			}
			if !streamed {
				// SecretNAT: display restore (when enabled) applies to the
				// emitted text only; assistantContent stays synthetic.
				o.emitOutput("text", o.natDisplayText(tb.Text)+"\n")
			}
			assistantContent.WriteString(tb.Text)
		}

		// If no tool calls and stop reason is end_turn, we're done
		if len(resp.ToolCalls) == 0 && resp.StopReason == provider.StopReasonEndTurn {
			o.phase = PhaseDone
			// Append assistant response to conversation
			if assistantContent.Len() > 0 {
				o.conversation = append(o.conversation, provider.ConversationTurn{
					Role:        "assistant",
					Content:     assistantContent.String(),
					Category:    provider.TurnCategoryAI,
					TimestampMs: time.Now().UnixMilli(),
					Thinking:    resp.ThinkingBlocks,
				})
			}
			return
		}

		// Process tool calls
		if len(resp.ToolCalls) > 0 {
			// Build assistant turn with tool calls
			assistantTurn := provider.ConversationTurn{
				Role:        "assistant",
				Content:     assistantContent.String(),
				ToolCalls:   make([]provider.ToolCallRequest, len(resp.ToolCalls)),
				Category:    provider.TurnCategoryAI,
				TimestampMs: time.Now().UnixMilli(),
				Thinking:    resp.ThinkingBlocks,
			}
			for i, tc := range resp.ToolCalls {
				assistantTurn.ToolCalls[i] = provider.ToolCallRequest{
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Input,
				}
			}
			o.conversation = append(o.conversation, assistantTurn)

			// Execute each tool call. Follow-up item 4: when the tool errored
			// with a typed Kind, prefix the conversation content with
			// "[error kind=X] ..." so the model can read it deterministically.
			//
			// Each result turn is categorized for the session JSON: "tool"
			// with the tool name as Origin, or "subagent" with the child
			// orchestrator id as Origin when the call was a sub_agent
			// delegation. Summary carries the short step title + digest so
			// low-bandwidth renderers never need the full Content.
			toolResults := make([]provider.ConversationTurn, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				result := o.executeTool(ctx, tc)
				toolResults = append(toolResults, o.buildToolResultTurn(tc, result))
				// A user interrupt during tool execution pauses the run. The
				// already-collected results are appended first so the
				// conversation stays consistent; remaining calls are closed
				// with synthetic results by the defer.
				if o.ctx.Err() != nil {
					o.conversation = append(o.conversation, toolResults...)
					o.markPaused(msg.StoppedReasonCancelled)
					return
				}
			}
			o.conversation = append(o.conversation, toolResults...)

			// A question was surfaced to the user this round (grounding_report
			// with understood=false, or an ask_user timeout escalation): PAUSE
			// with the checkpoint intact. The tool results above are already
			// appended, so the transcript is well-formed; the user's reply
			// arrives as the next prompt and resumes from exactly here.
			if o.pendingQuestion != "" {
				o.emitOutput("text", "\n[Paused: waiting for your answer — reply in this pane to resume]\n")
				o.markPaused(msg.StoppedReasonAwaitingInfo)
				return
			}
		} else if resp.StopReason == provider.StopReasonMaxTokens {
			// Max tokens - append what we have and continue
			if assistantContent.Len() > 0 {
				o.conversation = append(o.conversation, provider.ConversationTurn{
					Role:        "assistant",
					Content:     assistantContent.String(),
					Category:    provider.TurnCategoryAI,
					TimestampMs: time.Now().UnixMilli(),
					Thinking:    resp.ThinkingBlocks,
				})
			}
			o.emitOutput("text", "\n[Response truncated, continuing...]\n")
		}
	}
}

// markPaused records the interrupt checkpoint: phase, reason, and the
// user-facing hint. Idempotent — the first reason wins.
func (o *OrchestratorActor) markPaused(reason string) {
	if o.interrupted {
		return
	}
	o.interrupted = true
	o.stoppedReason = reason
	o.phase = PhasePaused
}

// pausedStepTitle renders the step-event title for a paused run.
func pausedStepTitle(reason string) string {
	switch reason {
	case msg.StoppedReasonCancelled:
		return "paused — interrupted by user"
	case msg.StoppedReasonMaxIterations:
		return "paused — iteration limit reached"
	case msg.StoppedReasonMaxDuration:
		return "paused — time limit reached"
	case msg.StoppedReasonMaxTokens:
		return "paused — token budget reached"
	case msg.StoppedReasonAwaitingInfo:
		return "paused — waiting for user answer"
	}
	return "paused"
}

// requestConversation returns the transcript for an outgoing LLM request: the
// stored append-only conversation plus, when a current page screenshot exists,
// ONE ephemeral trailing user turn carrying it. The ephemeral turn is never
// stored or persisted, so the stored prefix stays byte-stable and Anthropic's
// incremental prompt cache keeps hitting across rounds (the provider places
// its trailing cache breakpoint on the last STORED message for this reason).
func (o *OrchestratorActor) requestConversation() []provider.ConversationTurn {
	if o.latestScreenshot == nil {
		return o.conversation
	}
	out := make([]provider.ConversationTurn, len(o.conversation), len(o.conversation)+1)
	copy(out, o.conversation)
	out = append(out, provider.ConversationTurn{
		Role:          "user",
		Content:       "[current page screenshot — the live state after the tool results above]",
		ContentBlocks: []provider.ContentBlock{*o.latestScreenshot},
		Category:      provider.TurnCategorySystem,
		Origin:        "ephemeral-screenshot",
		TimestampMs:   time.Now().UnixMilli(),
	})
	return out
}

// callProvider performs one LLM call for the current iteration through the
// neutral ChatProvider boundary (design 002 A1/A2): the stored
// ConversationTurn transcript is converted to neutral Turns at this seam and
// the provider edge (AsChatProvider + the turns.go converters) converts back,
// so the orchestrator's provider boundary compiles against neutral types
// while its internal bookkeeping stays on the shim. Behavior is unchanged:
// the wire request is byte-identical to the direct AgenticProvider call, and
// streaming keys off the same StreamingProvider capability as before
// (ChatStreamSupported mirrors that type assertion for adapted providers).
//
// Phase 2 E: streaming is preferred when supported so the user sees
// token-incremental output instead of one block per turn; otherwise the
// one-shot path is used transparently.
func (o *OrchestratorActor) callProvider(providerSpecs []provider.ToolSpec) (resp *provider.AgenticResponse, streamed bool, err error) {
	chat := provider.AsChatProvider(o.prov)
	req := provider.ChatRequest{
		System: o.systemPrompt,
		Turns:  provider.TurnsFromConversation(o.requestConversation()),
		Tools:  providerSpecs,
	}

	// SecretNAT display restore (opt-in via snat.restore_display): the
	// streamed DISPLAY text has synthetic tokens restored to real values,
	// buffering across delta boundaries so a token split over chunks still
	// restores. The assistant turn appended to the conversation is built
	// from resp.TextBlocks by the caller and stays synthetic either way.
	var displayRestorer *secretnat.StreamRestorer
	if o.nat != nil && o.nat.Enabled() && o.nat.RestoreDisplay() {
		displayRestorer = o.nat.NewStreamRestorer()
	}

	var chatResp *provider.ChatResponse
	if provider.ChatStreamSupported(chat) {
		streamed = true
		// Tracks whether this provider call has streamed text yet, so the
		// status flip below fires once per call, not per delta.
		textStreamed := false
		chatResp, err = chat.ChatStream(
			o.ctx,
			req,
			func(ev provider.StreamEvent) {
				// Live-emit only text deltas; tool-use deltas are handled
				// once the final block is assembled (the model's tool
				// input must be valid JSON before dispatch).
				if ev.Type == provider.StreamEventTextDelta && ev.Text != "" {
					// Status accuracy while the grounding gate is closed: a
					// run that answers with read-only tools never calls
					// grounding_report, so the per-iteration status would
					// read "grounding" for the entire run — even while the
					// model is visibly streaming its answer, which looks
					// like a hang. Once text streams, the model is
					// PRODUCING output, so flip the status line to
					// "executing"; the next iteration re-emits "grounding"
					// if the gate is still closed (still exploring).
					// Safe to touch o.phase here: the streamer invokes this
					// callback synchronously on the run-loop goroutine.
					if o.grounding && !textStreamed {
						textStreamed = true
						o.emitStatus(PhaseExecuting)
					}
					if displayRestorer != nil {
						if txt := displayRestorer.Feed(ev.Text); txt != "" {
							o.emitOutput("text", txt)
						}
					} else {
						o.emitOutput("text", ev.Text)
					}
				}
			},
		)
		// Drain the display restorer's held-back tail (a partial-token
		// suffix) once the stream is over.
		if displayRestorer != nil {
			if tail := displayRestorer.Flush(); tail != "" {
				o.emitOutput("text", tail)
			}
		}
	} else {
		chatResp, err = chat.Chat(o.ctx, req)
	}
	return provider.AgenticResponseFromChat(chatResp), streamed, err
}

// (pruneScreenshotBlocks was removed: screenshots are no longer stored in the
// transcript at all — see latestScreenshot / requestConversation. Mutating
// earlier turns invalidated the incremental prompt cache every round.)

// buildToolResultTurn renders a tool's outcome as a categorized conversation
// turn. Sub-agent calls are categorized "subagent" (Origin = child
// orchestrator id from the tool output metadata); everything else is "tool"
// (Origin = tool name). Summary is the short step title plus the one-line
// result digest — the same strings the step-event stream carries.
func (o *OrchestratorActor) buildToolResultTurn(tc provider.ToolCallRequest, result *tools.ToolOutput) provider.ConversationTurn {
	turn := provider.ConversationTurn{
		Role:        "tool",
		Content:     surfaceToolResultContent(result),
		ToolCallID:  tc.ID,
		IsError:     result.Error != "",
		Category:    provider.TurnCategoryTool,
		Origin:      tc.Name,
		Summary:     stepTitle(tc.Name, tc.Input) + " — " + toolResultSummary(result),
		TimestampMs: time.Now().UnixMilli(),
	}
	// A browser screenshot rides the tool output as metadata. It is NOT
	// attached to the stored turn: the transcript stays append-only (mutating
	// it broke the incremental prompt cache — see latestScreenshot) and the
	// persisted session stays lean. Instead the newest capture replaces
	// o.latestScreenshot and is injected as an ephemeral trailing message on
	// the next request (requestConversation) so the model still SEES the page.
	if shot := result.Metadata["screenshot_base64"]; shot != "" {
		// Producers disagree on encoding (headless CDP → PNG, Electron embedded
		// browser → JPEG), and the Anthropic API 400s when the declared
		// media_type doesn't match the magic bytes — sniff, never assume.
		shot = provider.StripImageDataURLPrefix(shot)
		o.latestScreenshot = &provider.ContentBlock{
			Type: "image",
			Source: &provider.ImageSource{
				Type:      "base64",
				MediaType: provider.SniffImageMediaType(shot),
				Data:      shot,
			},
		}
	}
	if tc.Name == SubAgentToolName {
		turn.Category = provider.TurnCategorySubAgent
		if result.Metadata != nil {
			if id := result.Metadata["sub_agent_id"]; id != "" {
				turn.Origin = id
			}
			if title := result.Metadata["sub_agent_title"]; title != "" {
				turn.Summary = title + " — " + toolResultSummary(result)
			}
		}
	}
	return turn
}

// executeTool runs a single tool call with approval checks.
func (o *OrchestratorActor) executeTool(ctx actor.Context, tc provider.ToolCallRequest) *tools.ToolOutput {
	// Loop detection: block repeated identical tool calls.
	if o.detectLoop(tc.Name, tc.Input) {
		errMsg := fmt.Sprintf("Loop detected: tool %s has been called with the same parameters %d times. Try a different approach.", tc.Name, o.loopThreshold)
		o.emitOutput("error", errMsg+"\n")
		return tools.ErrOutput(tools.ErrKindLoopBlocked, errMsg)
	}

	// grounding_report is intercepted before normal tool dispatch (like
	// sub_agent below): understood=true opens the gate; understood=false
	// surfaces a question and schedules an awaiting-info pause. The tool's
	// registered Execute is intentionally never reached for this name.
	if tc.Name == GroundingToolName {
		return o.handleGroundingReport(tc)
	}

	// Grounding gate: while grounding, only read-only exploration tools may
	// run. Param-aware: browser_action is allowed for OBSERVE actions
	// (get_text/get_elements/screenshot/…) so web-mode panes can ground on
	// the live page, while mutating actions stay locked. The typed error
	// tells the model how to open the gate.
	if o.grounding && !groundingAllowedCall(tc.Name, tc.Input) {
		errMsg := groundingBlockedMessage(tc.Name)
		o.emitOutput("error", errMsg+"\n")
		return tools.ErrOutput(tools.ErrKindPermissionDenied, errMsg)
	}

	// sub_agent is intercepted before normal tool dispatch: it spawns a child
	// OrchestratorActor with its own context window and returns only the
	// child's summary to the parent conversation. The tool's registered
	// Execute is intentionally never reached for this name.
	if tc.Name == SubAgentToolName {
		return o.handleSubAgent(ctx, tc)
	}

	// Phase 3 N: fine-grained permissions. The policy can short-circuit a
	// tool call (deny) or skip the normal approval prompt (allow). When the
	// policy returns "ask" or there is no policy, control falls through to
	// the existing approval flow.
	if o.permissions != nil {
		switch o.permissions.Decide(tc.Name, tc.Input) {
		case PermDeny:
			errMsg := fmt.Sprintf("denied by permission policy: %s", tc.Name)
			o.emitOutput("error", errMsg+"\n")
			return tools.ErrOutput(tools.ErrKindPermissionDenied, errMsg)
		case PermAllow:
			// Mark this specific call auto-approved so executeWith* paths
			// skip the prompt. We do NOT add to autoApproved persistently
			// to avoid leaking policy decisions across runs.
		}
	}

	// Phase 3 L + follow-up item 7: stale-edit protection. For edit-shaped
	// tools, refuse if the file changed on disk since the model read it.
	// file_write is intentionally NOT gated — full-file writes have
	// replacement semantics. apply_patch goes through extractFilePaths
	// (plural) so a multi-file patch is gated per target.
	if isStaleCheckedTool(tc.Name) {
		for _, p := range extractFilePaths(tc.Name, tc.Input) {
			if stale := o.readTracker.staleCheck(p); stale != nil {
				o.emitOutput("error", stale.Error+"\n")
				return stale
			}
		}
	}

	executor, ok := o.tools.Get(tc.Name)
	if !ok {
		errMsg := fmt.Sprintf("unknown tool: %s", tc.Name)
		o.emitOutput("error", errMsg+"\n")
		return &tools.ToolOutput{Error: errMsg}
	}

	// Emit tool call to output
	o.emitToolCallHeader(toolCallHeader(tc.Name, tc.Input))
	o.emitStep(msg.StepToolStart, stepTitle(tc.Name, tc.Input), "", provider.TurnCategoryTool, tc.Name)

	// Check if approval is needed: the tool's own classifier, then policy-as-code
	// (design 013), then the session auto-approve registry and headless
	// auto-approve. A policy gate (always_gate / bash.deny) overrides the last
	// two — restrictive wins, so an unattended humanoid still gates.
	needsApproval, policyRule := o.decideApproval(tc.Name, tc.Input, executor.RequiresApproval(tc.Input))
	if policyRule != "" {
		verb := "auto-approved"
		if needsApproval {
			verb = "gated"
		}
		slog.Info("policy: tool call "+verb+" by rule", "tool", tc.Name, "rule", policyRule)
	}

	var output *tools.ToolOutput
	toolStart := time.Now()
	if needsApproval {
		// For file edits, we need to execute first to get the diff, then ask
		// approval. For destructive bash commands, we ask before execution.
		// Both helpers already apply Phase 2 K output shaping inline before
		// returning, so the shape step at the bottom is a no-op on their
		// output.
		if tc.Name == "edit" || tc.Name == "file_write" {
			output = o.executeWithPreview(ctx, tc, executor, policyRule)
		} else {
			output = o.executeWithPreApproval(ctx, tc, executor, policyRule)
		}
	} else {
		// Execute directly. SecretNAT: the input the model produced carries
		// synthetic tokens — restore real values just-in-time, on a transient
		// copy that never re-enters the conversation.
		out, execErr := executor.Execute(o.ctx, o.natRestoreInput(tc.Input))
		if execErr != nil {
			o.emitToolError(execErr.Error())
			result := tools.ErrOutputf(tools.ErrKindInternal, "tool %s failed: %v", tc.Name, execErr)
			// Follow-up item 3: record even on error so the dashboard sees it.
			result = o.natSanitizeOutput(result)
			o.metrics.RecordTool(tc.Name, time.Since(toolStart), result, execErr)
			return result
		}
		// Phase 2 K: cap oversized outputs. SecretNAT: sanitize at source so
		// display, metrics, and the tool_result turn only ever see tokens.
		output = o.natSanitizeOutput(shapeToolOutput(out))
		o.emitToolResult(tc.Name, output)
	}

	// Follow-up item 3: tool finished (with or without approval). Record
	// duration + output + error kind.
	o.metrics.RecordTool(tc.Name, time.Since(toolStart), output, nil)
	o.emitStep(msg.StepToolEnd, stepTitle(tc.Name, tc.Input)+" — "+toolResultSummary(output), "", provider.TurnCategoryTool, tc.Name)

	// Question escalation: a tool that timed out waiting for a human (e.g.
	// ask_user after its interactive window) can mark its output with
	// pause_on_timeout metadata — the run then pauses awaiting the answer
	// instead of discarding the question. See runLoop's pendingQuestion check.
	if output != nil && output.Metadata != nil && output.Metadata["pause_on_timeout"] == "true" {
		if q := strings.TrimSpace(output.Metadata["question"]); q != "" {
			o.pendingQuestion = q
			o.emitStep(msg.StepQuestion, questionDigest(q), q, provider.TurnCategorySystem, tc.Name)
		}
	}

	// Phase 3 L: record/refresh the read-tracker entry for file-shaped tools
	// AFTER a successful execution. file_read records; edit-shaped tools
	// refresh so the next edit sees the new state. Skipped when the tool
	// errored (output.Error != "").
	o.updateReadTrackerOnSuccess(tc.Name, tc.Input, output)

	return output
}

// isStaleCheckedTool returns true for tool names whose semantics are "edit
// an existing file" — i.e. tools that should refuse when the file has
// changed on disk since the model read it. file_write is intentionally NOT
// on the list (it has replace semantics). apply_patch IS gated (follow-up
// item 7) per-target by parsing the patch headers.
func isStaleCheckedTool(name string) bool {
	switch name {
	case "edit", "apply_patch":
		return true
	}
	return false
}

// updateReadTrackerOnSuccess records / refreshes the per-orchestrator read
// tracker after a tool completes successfully. The matrix:
//
//	file_read                       → Record (the model now knows this state)
//	file_edit / multi_edit          → Refresh (file changed by us; new baseline)
//	file_write                      → Refresh (created or replaced; new baseline)
//	apply_patch                     → Refresh per modify/create target;
//	                                  Forget per delete target. Follow-up item 7.
//
// Other tools are ignored. Errors on the output also skip the update
// (nothing changed on disk if the tool failed).
func (o *OrchestratorActor) updateReadTrackerOnSuccess(toolName string, params json.RawMessage, output *tools.ToolOutput) {
	if o.readTracker == nil || output == nil || output.Error != "" {
		return
	}
	switch toolName {
	case "file_read":
		if p := extractFilePath(params); p != "" {
			o.readTracker.Record(p)
		}
	case "edit", "file_write":
		if p := extractFilePath(params); p != "" {
			o.readTracker.Refresh(p)
		}
	case "apply_patch":
		// Multi-target: refresh per modify/create; forget per delete.
		var p struct {
			Patch string `json:"patch"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		targets, err := ParsePatchTargets(p.Patch)
		if err != nil {
			return
		}
		for _, t := range targets {
			switch t.Kind {
			case PatchModify, PatchCreate:
				o.readTracker.Refresh(t.Path)
			case PatchDelete:
				o.readTracker.Forget(t.Path)
			}
		}
	}
}

// executeWithPreview executes a file tool to generate the diff, then asks approval before applying.
// policyRule, when non-empty, is the policy-as-code rule that forced this gate (design 013); it is
// surfaced in the approval prompt for traceability.
func (o *OrchestratorActor) executeWithPreview(ctx actor.Context, tc provider.ToolCallRequest, executor tools.ToolExecutor, policyRule string) *tools.ToolOutput {
	// Execute to get the diff/preview. SecretNAT: restore real values for the
	// write itself (transient copy), then sanitize the resulting diff at
	// source so the preview shown for approval, the approval payload, and the
	// tool_result all carry tokens only.
	output, err := executor.Execute(o.ctx, o.natRestoreInput(tc.Input))
	if err != nil {
		errMsg := fmt.Sprintf("tool %s failed: %v", tc.Name, err)
		o.emitToolError(err.Error())
		return o.natSanitizeOutput(&tools.ToolOutput{Error: errMsg})
	}
	output = o.natSanitizeOutput(output)

	// Show the diff and ask for approval. The diff is ANSI-coloured for the
	// local terminal (green additions / red removals / cyan hunk headers); the
	// model's tool_result keeps the plain output.Content, and the shared output
	// stream is ANSI-stripped, so remote viewers still see readable text.
	o.emitOutput("diff", colorizeDiff(output.Content)+"\n")
	o.emitStatus(PhaseWaitingApproval)

	// Send approval request
	reqID := uuid.New().String()
	filePath := ""
	if output.Metadata != nil {
		filePath = output.Metadata["file_path"]
	}
	approvalReq := &msg.MsgApprovalRequest{
		RequestID:      reqID,
		OrchestratorID: o.id,
		ToolCallID:     tc.ID,
		Type:           msg.ApprovalTypeDiff,
		Description:    withPolicyRule(fmt.Sprintf("Apply changes to %s?", filePath), policyRule),
		Diff: &msg.DiffPayload{
			FilePath:    filePath,
			UnifiedDiff: output.Content,
		},
	}
	o.publishApprovalRequest(approvalReq)

	// Also emit to pane output for TUI/browser display
	o.emitApprovalPrompt("\n[y]es  [Y]es always  [n]o  [N]o + reason: ")
	o.emitStep(msg.StepApprovalWait, "waiting for approval: "+stepTitle(tc.Name, tc.Input), "", provider.TurnCategoryTool, tc.Name)

	// Wait for approval response (blocking in goroutine via channel)
	decision := o.waitForApproval(reqID)

	// Destroy ephemeral approval panes (no-op if approvalPaneGroups is empty)
	o.destroyApprovalPanes(reqID)

	// Interrupt while waiting → pause semantics, not a rejection.
	if decision.Reason == approvalInterruptedReason {
		o.emitOutput("text", "[Interrupted while waiting for approval — change not applied]\n")
		return interruptedApprovalOutput()
	}

	switch decision.Decision {
	case msg.DecisionYes:
		o.emitOutput("text", "[Approved]\n")
		o.emitStatus(PhaseExecuting)
		if filePath != "" {
			o.filesChanged = append(o.filesChanged, filePath)
		}
		return shapeToolOutput(output)

	case msg.DecisionYesAlways:
		o.emitOutput("text", "[Approved always]\n")
		approvalKey := o.buildApprovalKey(tc.Name, tc.Input)
		o.autoApproved[approvalKey] = true
		o.emitStatus(PhaseExecuting)
		if filePath != "" {
			o.filesChanged = append(o.filesChanged, filePath)
		}
		return shapeToolOutput(output)

	case msg.DecisionNo:
		o.emitOutput("text", "[Rejected]\n")
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{
			Content: "User rejected this change.",
			Error:   "rejected by user",
		}

	case msg.DecisionNoWithExplanation:
		reason := decision.Reason
		o.emitOutput("text", fmt.Sprintf("[Rejected: %s]\n", reason))
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{
			Content: fmt.Sprintf("User rejected this change. Reason: %s", reason),
			Error:   "rejected by user",
		}

	default:
		o.emitOutput("text", "[No response - skipping]\n")
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{
			Content: "No approval received, change not applied.",
			Error:   "no approval",
		}
	}
}

// executeWithPreApproval asks for approval before executing a potentially dangerous command.
// policyRule, when non-empty, is the policy-as-code rule that forced this gate (design 013).
func (o *OrchestratorActor) executeWithPreApproval(ctx actor.Context, tc provider.ToolCallRequest, executor tools.ToolExecutor, policyRule string) *tools.ToolOutput {
	o.emitToolCallHeader(toolCallHeader(tc.Name, tc.Input) + " · requires approval")
	o.emitStatus(PhaseWaitingApproval)

	reqID := uuid.New().String()
	approvalReq := &msg.MsgApprovalRequest{
		RequestID:      reqID,
		OrchestratorID: o.id,
		ToolCallID:     tc.ID,
		Type:           msg.ApprovalTypeDestructive,
		Description:    withPolicyRule(fmt.Sprintf("Execute potentially destructive command: %s", truncate(string(tc.Input), 100)), policyRule),
	}
	o.publishApprovalRequest(approvalReq)

	o.emitApprovalPrompt("\n[y]es  [Y]es always  [n]o  [N]o + reason: ")
	o.emitStep(msg.StepApprovalWait, "waiting for approval: "+stepTitle(tc.Name, tc.Input), "", provider.TurnCategoryTool, tc.Name)

	decision := o.waitForApproval(reqID)

	// Destroy ephemeral approval panes (no-op if approvalPaneGroups is empty)
	o.destroyApprovalPanes(reqID)

	// Interrupt while waiting → pause semantics, not a rejection.
	if decision.Reason == approvalInterruptedReason {
		o.emitOutput("text", "[Interrupted while waiting for approval — command not executed]\n")
		return interruptedApprovalOutput()
	}

	switch decision.Decision {
	case msg.DecisionYes, msg.DecisionYesAlways:
		if decision.Decision == msg.DecisionYesAlways {
			approvalKey := o.buildApprovalKey(tc.Name, tc.Input)
			o.autoApproved[approvalKey] = true
		}
		o.emitOutput("text", "[Approved]\n")
		o.emitStatus(PhaseExecuting)

		// SecretNAT: restore real values for execution (transient copy),
		// sanitize the output at source.
		output, err := executor.Execute(o.ctx, o.natRestoreInput(tc.Input))
		if err != nil {
			errMsg := fmt.Sprintf("tool %s failed: %v", tc.Name, err)
			o.emitToolError(err.Error())
			return o.natSanitizeOutput(&tools.ToolOutput{Error: errMsg})
		}
		output = o.natSanitizeOutput(shapeToolOutput(output))
		o.emitToolResult(tc.Name, output)
		return output

	case msg.DecisionNo:
		o.emitOutput("text", "[Rejected]\n")
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{Content: "User rejected this action.", Error: "rejected by user"}

	case msg.DecisionNoWithExplanation:
		o.emitOutput("text", fmt.Sprintf("[Rejected: %s]\n", decision.Reason))
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{
			Content: fmt.Sprintf("User rejected. Reason: %s", decision.Reason),
			Error:   "rejected by user",
		}

	default:
		o.emitOutput("text", "[No response - skipping]\n")
		o.emitStatus(PhaseExecuting)
		return &tools.ToolOutput{Content: "No approval received.", Error: "no approval"}
	}
}

// waitForApproval blocks until an approval response is received for the given request ID.
// It subscribes to the approval response subject and waits with a timeout.
func (o *OrchestratorActor) waitForApproval(requestID string) *msg.MsgApprovalResponse {
	subject := msg.T("pane", o.paneID, "approval", "response")
	ch := make(chan *msg.MsgApprovalResponse, 1)

	paneLogID := "pane:" + o.paneID
	if len(o.paneID) > 8 {
		paneLogID = "pane:" + o.paneID[:8]
	}

	sub, err := o.nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			if ml := msg.MsgLog(); ml.Enabled() {
				ml.LogError("orch waitForApproval decode", subject, err, paneLogID)
			}
			return
		}
		if env.TypeTag != msg.TagApprovalResponse {
			return
		}
		var resp msg.MsgApprovalResponse
		if err := json.Unmarshal(env.Payload, &resp); err != nil {
			if ml := msg.MsgLog(); ml.Enabled() {
				ml.LogError("orch waitForApproval decode response", subject, err, paneLogID)
			}
			return
		}
		if resp.RequestID == requestID {
			if ml := msg.MsgLog(); ml.Enabled() {
				ml.LogRecv(subject, env.TypeTag, paneLogID, false)
			}
			ch <- &resp
		}
	})
	if err != nil {
		return &msg.MsgApprovalResponse{Decision: msg.DecisionNo}
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Wait with timeout (5 minutes)
	select {
	case resp := <-ch:
		return resp
	case <-time.After(5 * time.Minute):
		return &msg.MsgApprovalResponse{Decision: msg.DecisionNo, Reason: "timeout"}
	case <-o.ctx.Done():
		// User interrupt while waiting: this is a PAUSE, not a rejection.
		// The caller detects approvalInterruptedReason and surfaces a
		// cancelled tool result so the model re-requests approval on resume.
		return &msg.MsgApprovalResponse{Decision: msg.DecisionNo, Reason: approvalInterruptedReason}
	}
}

// approvalInterruptedReason marks an approval wait that ended because the run
// was interrupted (double-Ctrl+C / replacement prompt), as opposed to the
// user answering "no". Callers translate it into an ErrKindCancelled tool
// result so a resumed run re-asks instead of treating the change as rejected.
const approvalInterruptedReason = "__interrupted__"

// interruptedApprovalOutput is the tool result recorded when an approval wait
// was cut short by an interrupt. Categorized as cancelled (not rejected) so
// the model re-issues the call — and the user re-approves — after "continue".
func interruptedApprovalOutput() *tools.ToolOutput {
	return tools.ErrOutput(tools.ErrKindCancelled,
		"interrupted while waiting for approval — the action was NOT applied; re-issue it after resume if still needed")
}

// handleApprovalResponse processes an approval response delivered via the actor mailbox.
func (o *OrchestratorActor) handleApprovalResponse(ctx actor.Context, m *msg.MsgApprovalResponse) {
	// This is handled by the waitForApproval subscription in the goroutine.
	// If we get it here via the actor mailbox, it means the bridge delivered it.
	// Re-publish to NATS so the goroutine subscription can pick it up.
	subject := msg.T("pane", o.paneID, "approval", "response")
	_ = o.pub.Send(subject, m)
}

// handleToolResult processes a tool result message.
func (o *OrchestratorActor) handleToolResult(_ actor.Context, _ *msg.MsgToolResult) {
	// For MVP, tool results are handled synchronously in the goroutine.
	// This handler is for future async tool execution.
}

func (o *OrchestratorActor) emitOutput(outputType, content string) {
	// Track whether the cumulative output currently sits at the start of a
	// line, so tool-call headers can be forced onto their own line (streamed
	// assistant text rarely ends in a newline). Empty emits don't move it.
	if content != "" {
		o.lastOutputNL = strings.HasSuffix(content, "\n")
	}

	// Nil publisher: state-only mode (unit tests construct bare
	// orchestrators). Emission is best-effort everywhere, so skipping is safe.
	if o.pub == nil {
		return
	}

	// Emit to the agentic output subject (used by Chrome extension and TUI).
	_ = o.pub.Send(o.outputSubject, &msg.MsgAgenticOutput{
		OrchestratorID: o.id,
		Type:           outputType,
		Content:        content,
	})

	// Emit via the unified ConversationMessage format to pane output topics.
	cm := &msg.ConversationMessage{
		TurnID:           o.id,
		TurnType:         msg.TurnAnswer,
		ConversationType: msg.ConvAI,
		InputType:        msg.InputPrompt,
		MessageSource:    msg.SourceAI,
		Content:          content,
		TimestampMs:      msg.NowMs(),
		SubjectToShare:   true,
	}

	if o.pipelineOutputSubject != "" {
		_ = o.pub.Send(o.pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: content})
	} else if o.chatOutputPaneID != "" {
		cm.ConversationType = msg.ConvChat
		cm.Role = "assistant"
		_ = o.pub.SendConversation(o.chatOutputPaneID, cm)
	} else {
		_ = o.pub.SendConversation(o.paneID, cm)
	}
}

// emitToolCallHeader emits a "⏺ tool(...)" header, guaranteeing it begins on a
// fresh line. Streamed assistant preamble text usually doesn't end in a
// newline, so without the leading separator the header would be appended to
// the end of that text (e.g. "...examining its structure. ⏺ bash(...)").
func (o *OrchestratorActor) emitToolCallHeader(line string) {
	o.emitOutput("tool_call", toolCallHeaderLine(line, o.lastOutputNL))
}

// toolCallHeaderLine frames a tool-call header: it prepends a newline when the
// stream is not already at the start of a line, and always terminates with one.
func toolCallHeaderLine(line string, atLineStart bool) string {
	if !atLineStart {
		line = "\n" + line
	}
	return line + "\n"
}

// emitConversationOutput publishes output using the unified ConversationMessage format.
func (o *OrchestratorActor) emitConversationOutput(content string, streaming bool) {
	cm := &msg.ConversationMessage{
		TurnID:           o.id,
		TurnType:         msg.TurnAnswer,
		ConversationType: msg.ConvAI,
		InputType:        msg.InputPrompt,
		MessageSource:    msg.SourceAI,
		Content:          content,
		TimestampMs:      msg.NowMs(),
		Streaming:        streaming,
		SubjectToShare:   true,
	}

	if o.chatOutputPaneID != "" {
		cm.ConversationType = msg.ConvChat
		cm.Role = "assistant"
		_ = o.pub.SendConversation(o.chatOutputPaneID, cm)
	} else if o.pipelineOutputSubject == "" {
		_ = o.pub.SendConversation(o.paneID, cm)
	}
}

func (o *OrchestratorActor) emitApprovalPrompt(text string) {
	cm := &msg.ConversationMessage{
		TurnID:           o.id,
		TurnType:         msg.TurnAnswer,
		ConversationType: msg.ConvAI,
		InputType:        msg.InputApproval,
		MessageSource:    msg.SourceSystem,
		Content:          text,
		TimestampMs:      msg.NowMs(),
	}

	if o.pipelineOutputSubject != "" {
		_ = o.pub.Send(o.pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: text})
	} else if o.chatOutputPaneID != "" {
		cm.ConversationType = msg.ConvChat
		cm.Role = "system"
		_ = o.pub.SendConversation(o.chatOutputPaneID, cm)
	} else {
		_ = o.pub.SendConversation(o.paneID, cm)
	}
}

func (o *OrchestratorActor) emitStatus(phase OrchestratorPhase) {
	o.phase = phase
	if o.pub == nil {
		return // state-only mode (unit tests)
	}
	_ = o.pub.Send(o.statusSubject, &msg.MsgAgenticStatus{
		OrchestratorID: o.id,
		Phase:          string(phase),
		Iteration:      o.iteration,
		MaxIterations:  o.maxIterations,
	})
	// Update pane status
	paneStatus := msg.T("pane", o.paneID, "status")
	statusText := fmt.Sprintf("[agentic] %s | iter %d/%d", phase, o.iteration, o.maxIterations)
	_ = o.pub.Send(paneStatus, &msg.MsgPaneStatusUpdate{Status: statusText})
}

// emitContextStatus publishes current context-window utilization to the pane
// status line so the user can see how full the conversation is.
func (o *OrchestratorActor) emitContextStatus() {
	if o.contextTokenLimit <= 0 || o.lastInputTokens <= 0 {
		return
	}
	pct := o.lastInputTokens * 100 / o.contextTokenLimit
	paneStatus := msg.T("pane", o.paneID, "status")
	statusText := fmt.Sprintf("[agentic] context %d%% (%d/%d tok) | iter %d/%d",
		pct, o.lastInputTokens, o.contextTokenLimit, o.iteration, o.maxIterations)
	// Prompt-cache hit rate: how much of the cumulative input was served from
	// Anthropic's cache (~10% price, excluded from the token budget). Low % on
	// a long run = the stored prefix is being invalidated = budget burning at
	// full price — the leash should be visibly healthy, not just present.
	if denom := o.cacheReadTokens + o.cacheWriteTokens + o.freshInputTokens; denom > 0 {
		statusText += fmt.Sprintf(" | cache %d%%", o.cacheReadTokens*100/denom)
	}
	_ = o.pub.Send(paneStatus, &msg.MsgPaneStatusUpdate{Status: statusText})
}

// emitUsage publishes one MsgUsageRecord per LLM call to the pane's usage
// subject, where the UsageActor (design 003) aggregates token/cost spend.
// Cost is left 0 here; the UsageActor prices records against its config-
// overridable pricing table so pricing lives in exactly one place. Empty
// (all-zero) usage is skipped. Safe in state-only mode (nil publisher).
func (o *OrchestratorActor) emitUsage(u provider.Usage) {
	if o.pub == nil {
		return
	}
	rec := o.usageRecord(u)
	if rec == nil {
		return
	}
	_ = o.pub.SendUsageRecord(rec)
}

// usageRecord builds the MsgUsageRecord for one LLM call, or nil for empty
// (all-zero) usage. Split out from emitUsage so the "attributes spend to the
// agent" behaviour is testable without a broker. AgentName is set only for a
// named agent/humanoid (empty for a pane), which is what lets the UsageActor
// answer `##cost week` by agent (design 003).
func (o *OrchestratorActor) usageRecord(u provider.Usage) *msg.MsgUsageRecord {
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return nil
	}
	model := ""
	if mp, ok := o.prov.(interface{ Model() string }); ok {
		model = mp.Model()
	}
	return &msg.MsgUsageRecord{
		PaneID:     o.paneID,
		AgentName:  o.agentName,
		Provider:   o.prov.Name(),
		Model:      model,
		Source:     msg.UsageSourceAgent,
		InTokens:   u.InputTokens,
		OutTokens:  u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		TS:         time.Now(),
	}
}

// maybeCompact summarizes and drops the oldest conversation turns when the most
// recent prompt size exceeds compactionThreshold of the context-token budget.
func (o *OrchestratorActor) maybeCompact() {
	if o.contextTokenLimit <= 0 || o.lastInputTokens <= 0 {
		return
	}
	threshold := int(float64(o.contextTokenLimit) * compactionThreshold)
	if o.lastInputTokens < threshold {
		return
	}
	if len(o.conversation) <= compactionKeepRecentTurns+1 {
		return // not enough history to be worth compacting
	}
	o.compactConversation()
}

// compactConversation replaces the oldest turns with a single synthetic summary
// turn, preserving the most recent turns verbatim. The run's original task turn
// is pinned verbatim inside the summary turn (see compactionSummaryContent) so
// its instructions survive. The cut point is chosen so the retained head never
// begins with an orphaned tool_result (which the Messages API rejects).
func (o *OrchestratorActor) compactConversation() {
	cut := o.safeCompactionCutIndex(len(o.conversation) - compactionKeepRecentTurns)
	if cut <= 0 {
		return // no safe boundary found; leave the conversation untouched
	}

	o.emitStatus(PhaseCompacting)
	o.emitOutput("text", "\n[Compacting earlier conversation to free context...]\n")
	o.emitStep(msg.StepCompaction, "compacting earlier conversation", "", provider.TurnCategorySystem, "")

	dropped := o.conversation[:cut]
	retained := o.conversation[cut:]
	compactStart := time.Now()
	summary := o.summarizeTurns(dropped)
	// Follow-up item 3: record how often + how slow compaction runs.
	o.metrics.RecordCompaction(len(dropped), time.Since(compactStart))

	newConv := make([]provider.ConversationTurn, 0, len(retained)+1)
	newConv = append(newConv, provider.ConversationTurn{
		Role:        "user",
		Content:     compactionSummaryContent(o.conversation[0], summary),
		Category:    provider.TurnCategorySystem,
		Origin:      "compaction",
		Summary:     "compacted earlier conversation",
		TimestampMs: time.Now().UnixMilli(),
	})
	newConv = append(newConv, retained...)
	o.conversation = newConv

	// Reset the running estimate; the next response reports the true size.
	o.lastInputTokens = 0
}

// compactionSummaryContent builds the synthetic summary-turn content, pinning
// the run's original task brief verbatim so instructions that only appear in
// the first prompt (output paths, naming conventions, formats) survive
// compaction. head is the conversation's first turn at compaction time:
//   - the run's first user prompt → pinned (truncated to pinnedTaskMaxChars);
//   - a previous compaction turn → its already-pinned block is carried forward,
//     so the brief survives any number of successive compactions;
//   - anything else → summary only.
func compactionSummaryContent(head provider.ConversationTurn, summary string) string {
	summaryBlock := compactionSummaryHeader + "\n\n" + summary
	if head.Origin == "compaction" {
		if idx := strings.Index(head.Content, compactionSummaryHeader); idx > 0 && strings.HasPrefix(head.Content, pinnedTaskHeader) {
			return strings.TrimSpace(head.Content[:idx]) + "\n\n" + summaryBlock
		}
		return summaryBlock
	}
	if head.Role != "user" || strings.TrimSpace(head.Content) == "" {
		return summaryBlock
	}
	task := head.Content
	if len(task) > pinnedTaskMaxChars {
		task = task[:pinnedTaskMaxChars] + "\n[...original task truncated...]"
	}
	return pinnedTaskHeader + "\n\n" + task + "\n\n" + summaryBlock
}

// safeCompactionCutIndex returns an index >= desired at which the conversation
// can be split without orphaning a tool_result. Tool-result turns ("tool") must
// stay attached to the assistant tool_use that precedes them, so the cut is
// advanced forward past any leading tool turns. Returns 0 if no safe cut keeps
// useful recent history.
func (o *OrchestratorActor) safeCompactionCutIndex(desired int) int {
	if desired <= 0 {
		return 0
	}
	cut := desired
	for cut < len(o.conversation) && o.conversation[cut].Role == "tool" {
		cut++
	}
	if cut >= len(o.conversation) {
		return 0
	}
	return cut
}

// summarizeTurns produces a compact natural-language summary of the dropped
// turns via the provider. On any failure it falls back to a mechanical digest
// so context pressure is still relieved.
func (o *OrchestratorActor) summarizeTurns(turns []provider.ConversationTurn) string {
	transcript := renderTranscript(turns)

	ctx, cancel := context.WithTimeout(o.ctx, 30*time.Second)
	defer cancel()

	// Template + {{transcript}} substitution. The template lives in
	// DefaultCompactionSummarizePrompt and is overridable by rysh-cli from its
	// embedded rysh-cli-agent-prompts/system_compaction_summarize.md.
	prompt := strings.ReplaceAll(DefaultCompactionSummarizePrompt, "{{transcript}}", transcript)
	if !strings.Contains(DefaultCompactionSummarizePrompt, "{{transcript}}") {
		// Defensive: if the template lacks the slot, append the transcript so
		// the call still has the data it needs.
		prompt = strings.TrimRight(DefaultCompactionSummarizePrompt, "\n") + "\n\nTranscript:\n" + transcript
	}

	if o.prov != nil {
		if summary, err := o.prov.Complete(ctx, prompt); err == nil && strings.TrimSpace(summary) != "" {
			return strings.TrimSpace(summary)
		}
	}
	return mechanicalDigest(turns)
}

// renderTranscript renders conversation turns into a plain-text transcript for
// summarization, truncating very long contents.
func renderTranscript(turns []provider.ConversationTurn) string {
	var sb strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "user":
			sb.WriteString("USER: ")
			sb.WriteString(truncate(t.Content, 2000))
			sb.WriteString("\n")
		case "assistant":
			if t.Content != "" {
				sb.WriteString("ASSISTANT: ")
				sb.WriteString(truncate(t.Content, 2000))
				sb.WriteString("\n")
			}
			for _, tc := range t.ToolCalls {
				sb.WriteString(fmt.Sprintf("ASSISTANT called tool %s(%s)\n", tc.Name, truncate(string(tc.Input), 300)))
			}
		case "tool":
			sb.WriteString("TOOL RESULT: ")
			sb.WriteString(truncate(t.Content, 800))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// mechanicalDigest is a no-LLM fallback summary listing turn counts and the set
// of tools used across the dropped turns.
func mechanicalDigest(turns []provider.ConversationTurn) string {
	var userMsgs, toolCalls int
	names := make([]string, 0)
	seen := map[string]bool{}
	for _, t := range turns {
		if t.Role == "user" {
			userMsgs++
		}
		for _, tc := range t.ToolCalls {
			toolCalls++
			if !seen[tc.Name] {
				seen[tc.Name] = true
				names = append(names, tc.Name)
			}
		}
	}
	return fmt.Sprintf("Earlier conversation: %d user message(s), %d tool call(s) using: %s.",
		userMsgs, toolCalls, strings.Join(names, ", "))
}

func (o *OrchestratorActor) emitToolResult(toolName string, output *tools.ToolOutput) {
	o.emitOutput("tool_result", toolResultBranch(toolResultSummary(output)))
}

// decideApproval resolves whether a tool call needs human approval, folding in
// policy-as-code (design 013). classifierRequires is the tool's own verdict
// (executor.RequiresApproval). Precedence, restrictive-wins:
//
//  1. Policy GATE (always_gate / bash.deny) → force approval, overriding BOTH
//     the per-session auto-approve registry AND headless auto-approve.
//  2. Policy AUTO (auto_approve / bash.allow) → skip approval.
//  3. Otherwise the tool's classifier, then the session registry / headless flag.
//
// Returns the resolved needsApproval and the matched policy rule ID ("" if none).
func (o *OrchestratorActor) decideApproval(toolName string, input json.RawMessage, classifierRequires bool) (bool, string) {
	needsApproval := classifierRequires
	forceGate := false
	policyRule := ""
	if dec, rid := evalApprovalPolicy(toolName, input); dec != PolicyDefault {
		policyRule = rid
		switch dec {
		case PolicyAutoApprove:
			needsApproval = false
		case PolicyGate:
			needsApproval = true
			forceGate = true
		}
	}
	// The session "approve all like this" registry and headless auto-approve are
	// both suppressed by a policy gate.
	if needsApproval && !forceGate {
		if o.autoApproved[o.buildApprovalKey(toolName, input)] {
			needsApproval = false
		}
	}
	if o.autoApproveAll && !forceGate {
		needsApproval = false
	}
	return needsApproval, policyRule
}

// withPolicyRule appends a policy-rule citation to an approval description so
// the user (and the audit record) sees which rule forced the gate.
func withPolicyRule(desc, policyRule string) string {
	if policyRule == "" {
		return desc
	}
	return desc + " [gated by policy rule " + policyRule + "]"
}

func (o *OrchestratorActor) buildApprovalKey(toolName string, params json.RawMessage) string {
	if toolName == "edit" || toolName == "file_write" || toolName == "file_read" {
		var fp struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(params, &fp) == nil && fp.FilePath != "" {
			return toolName + ":" + fp.FilePath
		}
	}
	if toolName == "bash" {
		var bp struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(params, &bp) == nil {
			parts := strings.Fields(bp.Command)
			if len(parts) > 0 {
				return toolName + ":" + parts[0]
			}
		}
	}
	return toolName
}

// toolCallLabel returns a short context string for display.
func toolCallLabel(toolName string, input json.RawMessage) string {
	switch toolName {
	case "bash":
		var p struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &p) == nil && p.Command != "" {
			return p.Command
		}
	case "edit", "file_write", "file_read":
		var p struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &p) == nil && p.FilePath != "" {
			return p.FilePath
		}
	case "glob":
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &p) == nil && p.Pattern != "" {
			if p.Path != "" {
				return p.Pattern + " in " + p.Path
			}
			return p.Pattern
		}
	case "grep":
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &p) == nil && p.Pattern != "" {
			if p.Path != "" {
				return p.Pattern + " in " + p.Path
			}
			return p.Pattern
		}
	case "web_search":
		var p struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(input, &p) == nil && p.Query != "" {
			return p.Query
		}
	case "web_fetch":
		var p struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(input, &p) == nil && p.URL != "" {
			return p.URL
		}
	case "page_context":
		return "reading page context"
	case "list_tools":
		return "listing tools"
	}
	return ""
}

// Markers for the structured tool-call rendering. A tool invocation is shown
// as a header line prefixed with toolCallMarker, and its outcome is rendered
// on the next line as an indented branch prefixed with toolResultMarker:
//
//	⏺ bash(go test ./...)
//	  ⎿  ✓ 12 lines
//
// This groups each call with its result and makes the two visually distinct
// (the old format prefixed both with "───", which read ambiguously).
const (
	toolCallMarker   = "⏺"
	toolResultMarker = "⎿"
)

// toolCallHeader renders a tool invocation header as "⏺ name(label)", falling
// back to "⏺ name" when the tool has no concise label. label is the same short
// context string (command, file path, query, …) used by toolCallLabel.
func toolCallHeader(toolName string, input json.RawMessage) string {
	label := toolCallLabel(toolName, input)
	if label == "" {
		return toolCallMarker + " " + toolName
	}
	return fmt.Sprintf("%s %s(%s)", toolCallMarker, toolName, label)
}

// toolResultBranch wraps a one-line summary as the indented result branch that
// sits beneath a tool-call header, e.g. "  ⎿  ✓ 3 lines\n".
func toolResultBranch(summary string) string {
	return fmt.Sprintf("  %s  %s\n", toolResultMarker, summary)
}

// toolResultSummary renders a tool's outcome as a single compact line: a check
// plus a line count on success ("✓ 3 lines", "✓ 1 line", or "✓" when there is
// no output) or a cross with the error — and the process exit code when one is
// reported — on failure.
func toolResultSummary(output *tools.ToolOutput) string {
	if output.Error != "" {
		errLine := truncate(firstLine(output.Error), 200)
		if output.ExitCode != 0 {
			return fmt.Sprintf("✗ exit %d: %s", output.ExitCode, errLine)
		}
		return "✗ " + errLine
	}
	content := strings.TrimRight(output.Content, "\n")
	if content == "" {
		return "✓"
	}
	n := strings.Count(content, "\n") + 1
	if n == 1 {
		return "✓ 1 line"
	}
	return fmt.Sprintf("✓ %d lines", n)
}

// emitToolError renders a failed tool execution as the indented result branch
// beneath its "⏺ tool(...)" header, matching the success-result layout.
func (o *OrchestratorActor) emitToolError(errMsg string) {
	o.emitOutput("error", toolResultBranch("✗ "+truncate(firstLine(errMsg), 200)))
}

// firstLine returns the first non-empty line of s, trimmed. Used to keep
// result/error summaries to a single line beneath the tool-call header.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (o *OrchestratorActor) buildSummary() string {
	var sb strings.Builder
	if o.interrupted {
		sb.WriteString(fmt.Sprintf("Paused (%s) — resumable with 'continue'. ", o.stoppedReason))
	}
	if len(o.filesChanged) > 0 {
		sb.WriteString(fmt.Sprintf("Changed %d file(s): %s. ", len(o.filesChanged), strings.Join(o.filesChanged, ", ")))
	}
	if len(o.errors) > 0 {
		sb.WriteString(fmt.Sprintf("Encountered %d error(s).", len(o.errors)))
	}
	if sb.Len() == 0 {
		sb.WriteString("Task completed.")
	}
	return strings.TrimSpace(sb.String())
}

// publishApprovalRequest sends an approval request either to the legacy approval
// subject (when approvalPaneGroups is empty) or creates ephemeral approval panes
// in each target pane group.
func (o *OrchestratorActor) publishApprovalRequest(req *msg.MsgApprovalRequest) {
	if len(o.approvalPaneGroups) == 0 {
		// Legacy behavior: publish to pane approval subject for TUI global mode.
		_ = o.pub.Send(o.approvalSubject, req)
		return
	}

	// New behavior: create ephemeral approval panes in each target group.
	responseSubject := msg.T("pane", o.paneID, "approval", "response")
	for _, groupID := range o.approvalPaneGroups {
		_ = o.pub.Send(
			msg.T("pane-group", groupID, "inbox"),
			&msg.MsgCreateApprovalPane{
				RequestID:       req.RequestID,
				SourcePaneID:    o.paneID,
				SourcePaneName:  o.paneName,
				OrchestratorID:  o.id,
				ApprovalRequest: req,
				ResponseSubject: responseSubject,
			},
		)
	}
}

// destroyApprovalPanes sends MsgDestroyApprovalPane to each target pane group
// to remove ephemeral approval panes for the given request ID.
func (o *OrchestratorActor) destroyApprovalPanes(requestID string) {
	for _, groupID := range o.approvalPaneGroups {
		_ = o.pub.Send(
			msg.T("pane-group", groupID, "inbox"),
			&msg.MsgDestroyApprovalPane{RequestID: requestID},
		)
	}
}

// handleSubAgent processes a `sub_agent` tool call by spawning a child
// OrchestratorActor with an isolated context window and a focused system
// prompt, then blocking until the child reports MsgOrchestratorDone. Only the
// child's summary is returned to the parent's conversation — the child's
// transcript is dropped, which is the entire point of sub-agents (context
// isolation).
//
// Recursive spawning is capped at MaxSubAgentDepth so a runaway sub-agent
// cannot consume unbounded actor budget.
func (o *OrchestratorActor) handleSubAgent(ctx actor.Context, tc provider.ToolCallRequest) *tools.ToolOutput {
	// Tool-call breadcrumb to parent's pane output so the user can see the
	// delegation happening.
	o.emitToolCallHeader(fmt.Sprintf("%s sub_agent(depth %d)", toolCallMarker, o.subAgentDepth+1))

	if o.subAgentDepth >= MaxSubAgentDepth {
		msgStr := fmt.Sprintf("sub_agent depth limit reached (max %d). Handle the remaining work inline.", MaxSubAgentDepth)
		o.emitOutput("error", msgStr+"\n")
		return tools.ErrOutput(tools.ErrKindValidation, msgStr)
	}

	p, err := ParseSubAgentParams(tc.Input)
	if err != nil {
		o.emitOutput("error", err.Error()+"\n")
		return tools.ErrOutput(tools.ErrKindValidation, err.Error())
	}

	systemPrompt := strings.TrimSpace(p.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = DefaultSubAgentSystemPrompt
	}
	childRegistry := BuildSubAgentRegistry(o.tools, p.AllowedTools)
	seedConv := BuildSubAgentSeedConversation(p)

	o.emitStep(msg.StepSubAgentStart, subAgentStepTitle(p.Task), "", provider.TurnCategorySubAgent, "")

	maxIter := DefaultSubAgentMaxIterations
	if o.maxIterations > 0 && o.maxIterations < maxIter*2 {
		// Keep the child cap proportional to a tight parent budget so a small
		// parent never spawns a child with more iterations than itself.
		half := o.maxIterations / 2
		if half < 5 {
			half = 5
		}
		if half < maxIter {
			maxIter = half
		}
	}

	childOrchID := NewSubAgentOrchID()
	req := newSubAgentSpawnReq(childOrchID, seedConv, systemPrompt, maxIter, o.subAgentDepth+1, childRegistry)

	// Hand the spawn off to the actor's Receive so ctx.Spawn happens inside
	// proto.actor's per-actor processing context.
	ctx.Send(ctx.Self(), req)

	// Wait for spawn outcome.
	select {
	case spawnErr := <-req.spawnErr:
		errMsg := fmt.Sprintf("sub_agent spawn failed: %v", spawnErr)
		o.emitOutput("error", errMsg+"\n")
		return tools.ErrOutput(tools.ErrKindInternal, errMsg)
	case <-req.spawnedCh:
		// proceed
	case <-o.ctx.Done():
		return tools.ErrOutput(tools.ErrKindCancelled, "cancelled before sub-agent could start")
	}

	// Wait for the child's MsgOrchestratorDone (routed via Receive into doneCh).
	var done *msg.MsgOrchestratorDone
	select {
	case done = <-req.doneCh:
	case <-time.After(DefaultSubAgentTimeout):
		errMsg := fmt.Sprintf("sub_agent timed out after %s", DefaultSubAgentTimeout)
		o.emitOutput("error", errMsg+"\n")
		return tools.ErrOutput(tools.ErrKindTimeout, errMsg)
	case <-o.ctx.Done():
		return tools.ErrOutput(tools.ErrKindCancelled, "cancelled while sub-agent was running")
	}

	// Carry the child's filesChanged up so the parent's final summary reflects
	// all writes performed under its umbrella.
	if len(done.FilesChanged) > 0 {
		o.filesChanged = append(o.filesChanged, done.FilesChanged...)
	}

	// Summarize the child's run: the full FormatSubAgentSummary goes to the
	// model (tool result Content); the one-line digest + title travel as
	// metadata so the conversation turn and the step stream stay compact.
	summary := FormatSubAgentSummary(done)
	digest := subAgentResultDigest(done)
	title := subAgentStepTitle(p.Task)
	meta := map[string]string{
		"sub_agent_id":     done.OrchestratorID,
		"sub_agent_title":  title,
		"sub_agent_digest": digest,
	}
	o.emitStep(msg.StepSubAgentEnd, title+" — "+digest, summary, provider.TurnCategorySubAgent, done.OrchestratorID)
	o.emitOutput("tool_result", toolResultBranch("✓"))
	if done.Success {
		return &tools.ToolOutput{Content: summary, Metadata: meta}
	}
	// Surface failure as a tool error so the model can decide whether to retry
	// inline or pivot. Content still contains the summary for context.
	return &tools.ToolOutput{
		Content:  summary,
		Error:    "sub-agent reported failure",
		Metadata: meta,
	}
}

// handleSubAgentSpawn services the subAgentSpawnReq self-message: it spawns
// the child OrchestratorActor (which must happen inside the actor's Receive
// goroutine, not the runLoop goroutine), registers the child's done-channel
// in pendingSubAgents, and signals the waiter via spawnedCh / spawnErr.
func (o *OrchestratorActor) handleSubAgentSpawn(ctx actor.Context, req *subAgentSpawnReq) {
	// Sub-agents inherit the parent's cancellation context so a parent cancel
	// propagates straight through.
	childOrch := NewOrchestratorActor(
		req.childOrchID,
		o.paneID,
		o.pub,
		o.nc,
		o.prov,
		req.registry,
		req.conversation,
		req.systemPrompt,
		o.autoApproved,
		o.ctx,
		req.maxIter,
		o.pipelineOutputSubject,
		o.chatOutputPaneID,
		o.approvalPaneGroups,
		o.paneName,
		o.contextTokenLimit,
		req.childDepth,
		o.agentName, // sub-agent spend attributes to the same agent
	)

	// Sub-agents inherit the grounding protocol in ADVISORY form only: they
	// are pre-scoped by the parent's delegation (task + context), so the
	// enforced read-only gate would just burn their tighter iteration budget.
	if o.groundingMode == GroundingPrompt || o.groundingMode == GroundingEnforced {
		childOrch.SetGroundingMode(GroundingPrompt)
	}

	// Sub-agents share the parent's SecretNAT session: they operate in the
	// same conversation context, so token↔value mappings must be consistent
	// across the delegation boundary. (o.prov is already the NAT-wrapped leg
	// provider — the child's HTTP boundary is covered too.)
	childOrch.SetSecretNAT(o.nat)

	props := actor.PropsFromProducer(func() actor.Actor { return childOrch })
	pid, err := ctx.SpawnNamed(props, "sub-orch-"+req.childOrchID[:8])
	if err != nil {
		req.spawnErr <- err
		return
	}

	// Register the waiter BEFORE acknowledging the spawn, so a fast-finishing
	// child cannot deliver MsgOrchestratorDone before we have somewhere to
	// route it.
	o.pendingSubAgents[req.childOrchID] = req.doneCh
	_ = pid // PID retained via actor system; we do not need to track it here.
	req.spawnedCh <- pid
}

// surfaceToolResultContent renders a tool result for inclusion as a `tool`
// turn in the conversation. When the tool errored with a typed Kind, the
// content is prefixed with "[error kind=X] " so the model can react to
// categories rather than parsing free text. Follow-up item 4.
func surfaceToolResultContent(out *tools.ToolOutput) string {
	if out == nil {
		return ""
	}
	if out.Error == "" {
		return out.Content
	}
	prefix := "[error] "
	if out.ErrorKind != "" {
		prefix = fmt.Sprintf("[error kind=%s] ", out.ErrorKind)
	}
	if out.Content != "" {
		return prefix + out.Error + "\n" + out.Content
	}
	return prefix + out.Error
}

// ShouldAutoRetry returns true when a tool error of the given kind is
// worth a one-shot transparent retry by the orchestrator. Today only
// `transient` qualifies — adding kinds requires a behavioural argument,
// not just a categorisation one. Follow-up item 4.
func ShouldAutoRetry(kind string) bool {
	return kind == tools.ErrKindTransient
}
