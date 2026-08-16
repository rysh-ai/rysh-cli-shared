// SPDX-License-Identifier: Apache-2.0

// Package agentic implements the agentic coding assistant actors.
// The LLMPromptExecutionActor manages conversation history and spawns OrchestratorActors
// for each user request. The OrchestratorActor runs the autonomous loop:
// call LLM, execute tools, request approval, repeat until done.
package agentic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-shared/bridge"
	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/secretnat"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// LLMPromptExecutionActor is the top-level agent manager for a pane.
// It receives user prompts, maintains conversation history, spawns
// orchestrators, and relays output to the pane's data-plane subjects.
type LLMPromptExecutionActor struct {
	id            string
	paneID        string
	maxIterations int
	pub           *msg.NATSPublisher
	nc            *nats.Conn
	br            *bridge.NATSBridge
	prov          provider.AgenticProvider
	tools         *tools.ToolRegistry

	// Session memory: the single owner of the user session's conversational
	// state — categorized turns, the pause/resume checkpoint, and throttled
	// KV persistence. See SessionMemory.
	session      *SessionMemory
	systemPrompt string

	// Active orchestrator
	activeOrch   *actor.PID
	activeOrchID string
	cancelFn     context.CancelFunc

	// pendingPrompt holds a prompt that arrived while an orchestrator was
	// still running. Orchestrators are serialized: rather than spawn a second
	// one (which would race the first and drop its accumulated work), the new
	// prompt is queued here and the running orchestrator is cancelled; when
	// its MsgOrchestratorDone arrives, its conversation is merged into the
	// session and THEN this queued prompt starts — so the follow-up always
	// sees the full context of what just happened. Last-prompt-wins: a newer
	// prompt overwrites an older queued one.
	pendingPrompt *msg.MsgAgenticPrompt

	// Auto-continue run budget (##web auto long runs). When armed, a run that
	// pauses on the per-leg iteration/wall-clock cap is resumed automatically
	// (a fresh leg over the merged conversation) until the task finishes, the
	// leg allowance runs out, or the deadline passes — then it pauses for a
	// manual continue. Set via MsgSetRunBudget; disarmed on a clean finish or a
	// user cancel. See handleSetRunBudget / autoContinueDecision.
	autoContinueArmed    bool
	autoContinuesLeft    int       // remaining automatic resumes
	autoContinueDeadline time.Time // zero = no wall-clock cap
	autoContinueMaxDur   time.Duration
	// runStepInterval overrides the per-leg step cap for the armed run (0 → the
	// actor's default maxIterations). runMaxContextTokens is the run's CUMULATIVE
	// token budget (0 → no token cap); runTokensUsed accumulates tokens across
	// legs, and each leg is capped at the remaining budget (runMaxContextTokens -
	// runTokensUsed). Accounting resets when the finalizer takes over.
	runStepInterval     int
	runMaxContextTokens int
	runTokensUsed       int
	// Prompt-cache accounting across the run's legs (MsgGetRunStatus).
	runCacheReadTokens  int
	runCacheWriteTokens int
	runFreshInputTokens int
	// runTokensUsedTotal is the run's WHOLE-LIFE token count: unlike
	// runTokensUsed (the budget-enforcement counter, which resets when the
	// finalizer takes over and zeroes on disarm) it only resets when a new
	// budget is ARMED — so post-run sampling (run report) and cancelled runs
	// still read the true total.
	runTokensUsedTotal int
	// sessionGrounded is set once any leg of the current run produced a
	// grounding_report, and downgrades ENFORCED grounding to advisory for
	// subsequent legs (see groundingModeForLegSticky). Reset when a new run
	// budget is armed, so each fresh ##auto pass re-grounds exactly once.
	sessionGrounded bool
	// runArmedAt is when the current budget was armed (zero = not armed) —
	// reported as the run's start time by MsgGetRunStatus (##auto <kind> runs).
	runArmedAt time.Time

	// finalizerPrompt runs once as a dedicated closing leg when the auto-continue
	// budget is exhausted (the task hit the iteration/time cap without finishing),
	// so a partial run still ends with a saved artifact + summary. finalizerPhase
	// guards against re-entry (the finalizer leg itself must not re-trigger one).
	finalizerPrompt string
	finalizerPhase  bool
	// Finalizer sub-budget (reserved budget_percent share), applied when the
	// finalizer takes over. runFinalizerMaxTokens is the finalizer's cumulative
	// token allowance (a fresh share; token accounting resets on takeover). 0
	// fields fall back to the built-in finalizer allowance.
	runFinalizerMaxIter   int
	runFinalizerMaxDur    time.Duration
	runFinalizerMaxTokens int
	// runAutoApprove auto-approves this run's tool calls (##web auto
	// step.auto_approve). Per-run; set from MsgSetRunBudget, cleared on
	// disarmRunBudget (run end), applied via SetAutoApproveAll on each leg.
	runAutoApprove bool
	// runOutputDir is the run's results directory (MsgSetRunBudget.OutputDir).
	// Per-run model/effort seats (MsgSetRunBudget): runModel/runEffort apply
	// to the main legs; the finalizer seat applies to the takeover leg and
	// falls back to the run seat. Empty → the provider's configured defaults.
	runModel           string
	runEffort          string
	runFinalizerModel  string
	runFinalizerEffort string
	// Restated in every synthetic continue/finalizer turn so the save location
	// survives context compaction on long runs.
	runOutputDir string

	// Auto-approval registry (tool:context → true)
	// autoApproved is the pane's session-scoped "approve all like this"
	// registry. Shared (not copied) with every orchestrator this actor spawns,
	// so an answer given in one turn still holds in the next.
	autoApproved *ApprovalMemory
	// autoApproveAll, when true, is propagated to every spawned orchestrator so
	// all tool calls run without an approval prompt (headless humanoids/agents,
	// and panes armed with MsgSetRunBudget.AutoApprovePersist — the fleet arm).
	// Unlike runAutoApprove it survives disarmRunBudget, so it holds across
	// runs until explicitly revoked.
	autoApproveAll bool

	// groundingMode is propagated to every spawned orchestrator: "" / off,
	// prompt (advisory protocol in the system prompt), or enforced (read-only
	// tool gate until grounding_report). See grounding.go. Panes default to
	// enforced, agents/humanoids to prompt — set by the host via
	// SetGroundingMode. groundingModeDefault remembers the host-configured
	// value so a runtime ##grounding override can be reported (and told apart
	// from the default) in MsgGroundingStateReply.
	groundingMode        string
	groundingModeDefault string

	// NATS subjects
	inboxSubject          string
	outputSubject         string
	statusSubject         string
	pipelineOutputSubject string // if non-empty, also publish output here

	// Chat output routing (for autonomous agents).
	// When non-empty, output is sent to this pane's chat buffer instead of
	// the default AI output. Updated at runtime via MsgSetChatOutputPane.
	chatOutputPaneID string

	// Approval pane groups: when non-empty, approval requests are shown as
	// ephemeral panes in these pane groups instead of global approval mode.
	approvalPaneGroups []string
	// paneName is the display name shown in ephemeral approval pane titles.
	paneName string
	// agentName is set (via SetAgentName) only when this actor serves a named
	// autonomous agent or humanoid, so its usage records attribute to that agent
	// for `##cost` "by agent" (design 003). Empty for a regular pane.
	agentName string

	// Memory state — injected by the MemoryManager via MsgMemoryStateUpdate.
	// When non-nil, memory entries are prepended to the system prompt before
	// spawning orchestrators.
	memoryState *msg.MemoryState

	// contextTokenLimit is the input-token budget passed to spawned
	// orchestrators for context-window compaction. Defaults to
	// DefaultContextTokenLimit; override with SetContextTokenLimit.
	contextTokenLimit int

	// metrics is threaded into spawned orchestrators (follow-up 3b). Nil →
	// each orchestrator uses its own NoopMetricsSink (no recording).
	metrics MetricsSink

	// cwdResolver, when set, returns the working directory AI-mode tools should
	// run in — typically the pane's live shell cwd. It's threaded into every
	// spawned orchestrator, which evaluates it once per run so tools follow the
	// shell after a `cd`. Nil → tools keep their construction-time workDir.
	cwdResolver func() string

	// envBlockProvider, when set, returns a freshly-rendered environment block
	// (working directory, git, project layout, …) appended to the system prompt
	// for each prompt. It is evaluated per turn so the block reflects the live
	// shell cwd after a `cd`. The template lives in rysh-cli's prompt files; the
	// host (rysh-cli) builds the closure. Nil → no env block is appended.
	envBlockProvider func() string

	// scopeResolver, when set, maps a prompt's ScopeHint to the parent registry
	// this actor's tool registry should chain to for that run (the invoking
	// pane's scope chain). Evaluated per prompt so an agent inherits the scope of
	// wherever it was invoked. The host (rysh-cli) builds the closure over its
	// scope service. Nil → the registry's parent is left as-is (panes).
	scopeResolver func(scopeHint string) *tools.ToolRegistry

	// nat, when set, is this conversation's SecretNAT (ReSet) session: user
	// turns are sanitized before they reach session memory, every leg
	// provider is wrapped so no real secret crosses the HTTP boundary, and
	// spawned orchestrators restore/sanitize at the tool seam. Nil → off.
	nat secretnat.SessionHandle
}

// SetMetricsSink installs a metrics sink that's threaded into every
// orchestrator this actor spawns. Pass nil to disable recording on
// future runs. Follow-up 3b.
func (a *LLMPromptExecutionActor) SetMetricsSink(s MetricsSink) { a.metrics = s }

// SetCwdResolver installs a working-directory resolver threaded into every
// orchestrator this actor spawns. The resolver is evaluated at the start of
// each run; a non-empty result re-points all working-directory-aware tools at
// that path. Pass nil to disable (tools keep their default workDir).
func (a *LLMPromptExecutionActor) SetCwdResolver(fn func() string) { a.cwdResolver = fn }

// SetEnvBlockProvider installs a per-turn environment-block provider appended
// to the effective system prompt before each prompt. Pass nil to disable.
func (a *LLMPromptExecutionActor) SetEnvBlockProvider(fn func() string) { a.envBlockProvider = fn }

// SetScopeResolver installs a per-prompt scope resolver. Given a prompt's
// ScopeHint it returns the parent registry this actor's tool registry should
// chain to for that run (so an agent inherits the invoking pane's scope). Pass
// nil to disable. Used for agents/humanoids; panes leave it nil.
func (a *LLMPromptExecutionActor) SetScopeResolver(fn func(scopeHint string) *tools.ToolRegistry) {
	a.scopeResolver = fn
}

// SetSecretNAT installs this conversation's SecretNAT session, threaded into
// every leg provider and every spawned orchestrator. Pass nil to disable.
func (a *LLMPromptExecutionActor) SetSecretNAT(s secretnat.SessionHandle) { a.nat = s }

// SetAgentName marks this actor as serving a named autonomous agent or humanoid,
// so its usage records are attributed to that agent (design 003 "by agent").
// Regular panes never call this, so their records carry an empty AgentName.
func (a *LLMPromptExecutionActor) SetAgentName(name string) { a.agentName = name }

// NewLLMPromptExecutionActor creates a new LLMPromptExecutionActor for the given pane.
func NewLLMPromptExecutionActor(
	paneID string,
	maxIterations int,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	prov provider.AgenticProvider,
	toolRegistry *tools.ToolRegistry,
	systemPrompt string,
	pipelineOutputSubject string,
	chatOutputPaneID string,
	kvStore nats.KeyValue,
) *LLMPromptExecutionActor {
	id := uuid.New().String()
	if maxIterations <= 0 {
		maxIterations = 20
	}
	return &LLMPromptExecutionActor{
		id:                    id,
		paneID:                paneID,
		maxIterations:         maxIterations,
		pub:                   pub,
		nc:                    nc,
		prov:                  prov,
		tools:                 toolRegistry,
		systemPrompt:          systemPrompt,
		session:               NewSessionMemory(paneID, kvStore),
		autoApproved:          NewApprovalMemory(),
		inboxSubject:          msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
		outputSubject:         msg.T("pane", paneID, "llm_prompt_execution", "output"),
		statusSubject:         msg.T("pane", paneID, "llm_prompt_execution", "status"),
		pipelineOutputSubject: pipelineOutputSubject,
		chatOutputPaneID:      chatOutputPaneID,
		contextTokenLimit:     DefaultContextTokenLimit,
	}
}

// SetContextTokenLimit overrides the input-token budget used for context
// compaction. A value <= 0 resets it to DefaultContextTokenLimit.
func (a *LLMPromptExecutionActor) SetContextTokenLimit(limit int) {
	if limit <= 0 {
		limit = DefaultContextTokenLimit
	}
	a.contextTokenLimit = limit
}

// Receive handles messages for the LLMPromptExecutionActor.
func (a *LLMPromptExecutionActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.br = bridge.New(a.nc, ctx.Self(), ctx.ActorSystem(), a.pub.Codecs())
		a.br.SetPaneID(a.paneID)
		if err := a.br.AddSubject(a.inboxSubject); err != nil {
			slog.Error("agentic: subscribe inbox", "err", err)
		}
		// Also subscribe to approval responses
		approvalSubject := msg.T("pane", a.paneID, "approval", "response")
		if err := a.br.AddSubject(approvalSubject); err != nil {
			slog.Error("agentic: subscribe approval response", "err", err)
		}
		// Self-restore session memory (turns + pause checkpoint) from KV so a
		// paused run survives daemon restarts and can still be continued.
		if a.session.Restore() {
			paused, reason := a.session.Paused()
			slog.Info("agentic: session restored", "pane", a.paneID, "turns", a.session.Len(), "paused", paused, "reason", reason)
		}
		// Re-apply a persisted ##grounding override on top of the host default
		// (the host sets the default via SetGroundingMode before spawn, so the
		// ordering here is guaranteed).
		if ov := a.session.GroundingOverride(); ov != "" && ValidGroundingMode(ov) {
			a.groundingMode = ov
			slog.Info("agentic: grounding override restored", "pane", a.paneID,
				"mode", ov, "default", a.groundingModeDefault)
		}
		slog.Info("agentic: started", "pane", a.paneID)

	case *actor.Stopping:
		a.session.Persist(true)
		if a.cancelFn != nil {
			a.cancelFn()
		}
		if a.br != nil {
			a.br.Stop()
		}

	case *msg.MsgAgenticPrompt:
		a.handlePrompt(ctx, m)

	case *msg.MsgAgenticCancel:
		a.handleCancel()

	case *msg.MsgAgenticContinue:
		a.handleContinue(ctx)

	case *msg.MsgOrchestratorDone:
		a.handleOrchestratorDone(ctx, m)

	case *msg.MsgApprovalResponse:
		// Forward to active orchestrator
		if a.activeOrch != nil {
			ctx.Send(a.activeOrch, m)
		}

	case *msg.MsgSetRunBudget:
		a.handleSetRunBudget(m)

	case *msg.MsgSetChatOutputPane:
		a.chatOutputPaneID = m.PaneID
		slog.Info("agentic: chat output pane updated", "pane", a.paneID, "chatOutputPaneID", m.PaneID)

	case *msg.MsgPaneSetApprovalPaneGroups:
		a.approvalPaneGroups = m.PaneGroupIDs
		if m.PaneName != "" {
			a.paneName = m.PaneName
		}
		slog.Info("agentic: approval pane groups updated", "pane", a.paneID, "groups", m.PaneGroupIDs, "paneName", a.paneName)

	case *msg.MsgMemoryStateUpdate:
		a.memoryState = m.State
		slog.Info("agentic: memory state updated", "pane", a.paneID, "entries", len(m.State.Entries))

	case *msg.MsgReloadPrompts:
		// Follow-up 2b: swap the cached system prompt so the NEXT user
		// prompt picks up the new content. The currently-running
		// orchestrator (if any) keeps its captured snapshot — by design.
		if m.SystemPrompt != "" {
			a.systemPrompt = m.SystemPrompt
		}
		slog.Info("agentic: prompts reloaded", "pane", a.paneID)

	case *msg.MsgRestoreConversation:
		// Only adopt an external restore when it adds turns we don't have:
		// the actor self-restores from KV on Started, and clobbering that
		// state would drop the pause checkpoint.
		if len(m.Conversation) > a.session.Len() {
			a.session.Replace(m.Conversation)
			slog.Info("agentic: conversation restored", "pane", a.paneID, "turns", len(m.Conversation))
		}

	case *msg.MsgGetConversationHistory:
		a.handleGetConversationHistory(ctx, m, nil)

	case *msg.MsgSessionMemoryReplace:
		a.handleSessionMemoryReplace(ctx, m)

	case *msg.MsgSetGroundingMode:
		a.handleSetGroundingMode(m)

	case *msg.RequestEnvelope:
		// Unwrap request envelopes
		switch inner := m.Inner.(type) {
		case *msg.MsgAgenticPrompt:
			a.handlePrompt(ctx, inner)
		case *msg.MsgAgenticCancel:
			a.handleCancel()
		case *msg.MsgAgenticContinue:
			a.handleContinue(ctx)
		case *msg.MsgApprovalResponse:
			if a.activeOrch != nil {
				ctx.Send(a.activeOrch, inner)
			}
		case *msg.MsgGetConversationHistory:
			a.handleGetConversationHistory(ctx, inner, m)
		case *msg.MsgGetSessionMemory:
			a.handleGetSessionMemory(m)
		case *msg.MsgSessionMemoryReplace:
			a.handleSessionMemoryReplace(ctx, inner)
		case *msg.MsgSetGroundingMode:
			a.handleSetGroundingMode(inner)
		case *msg.MsgGetGroundingState:
			a.handleGetGroundingState(m)
		case *msg.MsgGetRunStatus:
			a.handleGetRunStatus(m)
		case *msg.MsgRestoreConversation:
			if len(inner.Conversation) > a.session.Len() {
				a.session.Replace(inner.Conversation)
				slog.Info("agentic: conversation restored (envelope)", "pane", a.paneID, "turns", len(inner.Conversation))
			}
		}
	}
}

// handleGetRunStatus replies with the live run/budget accounting behind
// `##auto <kind> runs`: tokens consumed across completed legs, the armed
// ceilings, and whether a leg is executing right now. Armed=false +
// InFlight=false tells the caller the run ended (prune the registry entry).
func (a *LLMPromptExecutionActor) handleGetRunStatus(env *msg.RequestEnvelope) {
	_ = env.Reply(a.buildRunStatusReply())
}

// buildRunStatusReply snapshots the run accounting fields into a reply.
// Pure (no actor.Context / NATS) so the mapping is unit-testable.
func (a *LLMPromptExecutionActor) buildRunStatusReply() *msg.MsgRunStatusReply {
	reply := &msg.MsgRunStatusReply{
		PaneID:           a.paneID,
		Armed:            a.autoContinueArmed,
		InFlight:         a.activeOrchID != "",
		Finalizer:        a.finalizerPhase,
		TokensUsed:       a.runTokensUsed,
		MaxContextTokens: a.runMaxContextTokens,
		StepInterval:     a.runStepInterval,
		ContinuesLeft:    a.autoContinuesLeft,
		CacheReadTokens:  a.runCacheReadTokens,
		CacheWriteTokens: a.runCacheWriteTokens,
		FreshInputTokens: a.runFreshInputTokens,
		TokensUsedTotal:  a.runTokensUsedTotal,
	}
	if a.session != nil {
		reply.Paused, reply.PausedReason = a.session.Paused()
	}
	if !a.runArmedAt.IsZero() {
		reply.ArmedAtMs = a.runArmedAt.UnixMilli()
	}
	if !a.autoContinueDeadline.IsZero() {
		reply.DeadlineMs = a.autoContinueDeadline.UnixMilli()
	}
	return reply
}

// handleGetSessionMemory replies with the FULL-FIDELITY session memory export
// (raw turns + pause checkpoint) for the ##hop fork flow. The reply is
// serialized over NATS, so the receiver gets its own copy — no aliasing of
// this actor's live slice.
func (a *LLMPromptExecutionActor) handleGetSessionMemory(env *msg.RequestEnvelope) {
	paused, reason := a.session.Paused()
	_ = env.Reply(&msg.MsgSessionMemoryReply{
		PaneID:       a.paneID,
		Turns:        a.session.Turns(),
		Paused:       paused,
		PausedReason: reason,
	})
}

// handleSessionMemoryReplace atomically replaces this actor's session memory
// with the carried state (##hop fork, replace semantics).
//
// Order matters: any in-flight orchestrator is interrupted FIRST and
// activeOrchID is cleared, so its late MsgOrchestratorDone is dropped by the
// stale-orchestrator guard instead of clobbering the freshly forked memory
// with the old run's conversation. The forked state (including the source's
// pause checkpoint) is then installed, healed, and force-persisted so the
// fork survives a daemon restart.
func (a *LLMPromptExecutionActor) handleSessionMemoryReplace(ctx actor.Context, m *msg.MsgSessionMemoryReplace) {
	if a.cancelFn != nil {
		a.cancelFn()
		a.cancelFn = nil
	}
	if a.activeOrch != nil {
		ctx.Stop(a.activeOrch)
		a.activeOrch = nil
	}
	a.activeOrchID = ""

	a.session.Replace(m.Turns)
	if m.Paused {
		a.session.Pause(m.PausedReason)
	} else {
		a.session.ClearPause()
	}
	a.session.Bound()
	a.session.Persist(true)

	slog.Info("agentic: session memory replaced (hop fork)",
		"pane", a.paneID, "source", m.SourcePaneID, "sourceAlias", m.SourceAlias,
		"turns", len(m.Turns), "paused", m.Paused)
}

func (a *LLMPromptExecutionActor) handlePrompt(ctx actor.Context, m *msg.MsgAgenticPrompt) {
	// If an orchestrator is still running, DON'T spawn a second one alongside
	// it — that races the running run and its late completion would be dropped,
	// losing every tool result it produced (e.g. a follow-up "save what you
	// found" would run against an empty conversation). Instead queue this
	// prompt and cancel the running orchestrator; handleOrchestratorDone will
	// merge the finished run's conversation into the session and then start the
	// queued prompt with full context. Cancel via the context only (not
	// ctx.Stop): the runLoop's deferred MsgOrchestratorDone must still reach us
	// so its work is captured. Last-prompt-wins: overwrite any older queued
	// prompt.
	if a.activeOrch != nil {
		a.pendingPrompt = m
		if a.cancelFn != nil {
			a.cancelFn()
			a.cancelFn = nil
		}
		slog.Info("agentic: queued prompt behind in-flight run", "pane", a.paneID)
		return
	}

	a.startPrompt(ctx, m)
}

// startPrompt appends the user turn and spawns an orchestrator for it. The
// caller guarantees no orchestrator is currently active (handlePrompt queues
// otherwise; handleOrchestratorDone calls this only after clearing the
// finished run).
func (a *LLMPromptExecutionActor) startPrompt(ctx actor.Context, m *msg.MsgAgenticPrompt) {
	// Any new prompt resumes a paused session implicitly: the preserved
	// conversation IS the checkpoint, so the fresh orchestrator picks up
	// exactly where the paused run stopped (typing "continue" is just the
	// simplest such prompt).
	if paused, reason := a.session.Paused(); paused {
		slog.Info("agentic: resuming paused session", "pane", a.paneID, "reason", reason)
		a.session.ClearPause()
	}

	// SecretNAT: sanitize the user turn BEFORE it reaches session memory, so
	// user-typed secrets never land in the persisted conversation (KV). Text
	// content blocks are sanitized too; image blocks pass through. The NAT
	// provider decorator would catch these at the HTTP boundary anyway —
	// sanitizing here keeps the on-disk transcript clean as well.
	if a.nat != nil && a.nat.Enabled() {
		m.Prompt = a.nat.Sanitize(m.Prompt)
		for i := range m.ContentBlocks {
			if m.ContentBlocks[i].Type == "text" && m.ContentBlocks[i].Text != "" {
				m.ContentBlocks[i].Text = a.nat.Sanitize(m.ContentBlocks[i].Text)
			}
		}
	}

	// Append the categorized user turn. Follow-up 1b: when ContentBlocks
	// is non-empty (e.g. an image attached via ##image <path>), the
	// provider's buildMessages emits the structured Anthropic array form
	// and Content becomes a leading text block.
	a.session.AppendUserTurn(m.Prompt, m.ContentBlocks)

	// Trim + heal both ends of the window (head: no orphaned tool_result;
	// tail: no unanswered tool_use from an interrupted run).
	a.session.Bound()

	// Resolve the scope chain for this run: an agent/humanoid re-points its tool
	// registry's parent to the invoking pane's scope chain (per the ScopeHint),
	// so it sees exactly the tools enabled at that pane's scope. Done before the
	// orchestrator clones a.tools (same goroutine, so the clone observes it).
	if a.scopeResolver != nil {
		if parent := a.scopeResolver(m.ScopeHint); parent != nil {
			a.tools.SetParent(parent)
		}
	}

	// Build effective system prompt: base prompt + memory context (if available).
	effectivePrompt := a.buildEffectiveSystemPrompt()

	// Spawn orchestrator
	orchID := uuid.New().String()
	orchCtx, cancelFn := context.WithCancel(context.Background())
	a.cancelFn = cancelFn
	a.activeOrchID = orchID

	maxIter := a.maxIterations
	if maxIter <= 0 {
		maxIter = 20
	}
	// When a run budget is armed, each leg uses the recipe's step_interval as its
	// per-leg step cap (checkpoint cadence), overriding the actor default.
	if a.autoContinueArmed && a.runStepInterval > 0 {
		maxIter = a.runStepInterval
	}

	orch := NewOrchestratorActor(
		orchID,
		a.paneID,
		a.pub,
		a.nc,
		a.legProvider(),
		a.tools,
		a.session.Turns(),
		effectivePrompt,
		a.autoApproved,
		orchCtx,
		maxIter,
		a.pipelineOutputSubject,
		a.chatOutputPaneID,
		a.approvalPaneGroups,
		a.paneName,
		a.contextTokenLimit,
		0, // top-level orchestrator: sub-agent depth starts at 0
		a.agentName,
	)
	// Follow-up 3b: thread the metrics sink so this orchestrator records
	// LLM/tool/compaction events. Skipped (no-op) when SetMetricsSink
	// was never called on this actor.
	if a.metrics != nil {
		orch.SetMetricsSink(a.metrics)
	}
	// Thread the cwd resolver so the orchestrator points tools at the pane's
	// live shell cwd at the start of its run.
	if a.cwdResolver != nil {
		orch.SetCwdResolver(a.cwdResolver)
	}
	// Thread the SecretNAT session so the orchestrator restores real values
	// at tool execution and sanitizes tool outputs at source.
	if a.nat != nil {
		orch.SetSecretNAT(a.nat)
	}
	// Headless auto-approval (humanoids/agents): every tool runs without a prompt.
	if a.autoApproveAll || a.runAutoApprove {
		orch.SetAutoApproveAll(true)
	}
	// Grounding protocol: read the codebase like a human before acting. The
	// finalizer/takeover leg skips it: it continues an already-grounded
	// conversation and must spend its reserved budget on wrapping up, not on
	// re-earning tool access through the grounding gate.
	if mode := groundingModeForLegSticky(a.groundingMode, a.finalizerPhase, a.sessionGrounded); mode != "" {
		orch.SetGroundingMode(mode)
	}
	// Auto-continue: cap this leg's wall-clock at the time remaining to the
	// overall deadline so no single leg overshoots the run's max_duration budget.
	if a.autoContinueArmed && !a.autoContinueDeadline.IsZero() {
		if remaining := time.Until(a.autoContinueDeadline); remaining > 0 {
			orch.SetMaxDuration(remaining)
		}
	}
	// Cumulative token cap: cap this leg at the run's REMAINING token budget
	// (total minus what prior legs used), so the budget is enforced across legs.
	if a.autoContinueArmed && a.runMaxContextTokens > 0 {
		if remaining := a.runMaxContextTokens - a.runTokensUsed; remaining > 0 {
			orch.SetMaxContextTokens(remaining)
		}
	}

	props := actor.PropsFromProducer(func() actor.Actor { return orch })
	pid, err := ctx.SpawnNamed(props, "orch-"+orchID[:8])
	if err != nil {
		slog.Error("agentic: spawn orchestrator", "err", err)
		a.emitOutput(orchID, "error", fmt.Sprintf("Failed to start orchestrator: %v", err))
		return
	}
	a.activeOrch = pid

	// Emit status
	a.emitStatus(orchID, "planning", 0, maxIter)
}

// handleCancel interrupts the in-flight run with PAUSE semantics: the
// orchestrator's context is cancelled, but the actor is NOT stopped — its
// runLoop defer still delivers MsgOrchestratorDone (Interrupted=true) with the
// preserved conversation, which handleOrchestratorDone records as a resumable
// checkpoint. Triggered by double-Ctrl+C in the TUI and `@@name stop`.
func (a *LLMPromptExecutionActor) handleCancel() {
	if a.cancelFn != nil {
		a.cancelFn()
		a.cancelFn = nil
	}
	if a.activeOrchID != "" {
		a.emitOutput(a.activeOrchID, "text", "\n[Interrupted — state preserved; type 'continue' to resume]\n")
	}
}

// handleContinue resumes a paused session programmatically (the TUI's
// continue affordance, or `@@name continue` for agents/humanoids). It is
// equivalent to the user typing "continue" as a prompt. A continue on a
// session that isn't paused is ignored with a notice.
func (a *LLMPromptExecutionActor) handleContinue(ctx actor.Context) {
	paused, _ := a.session.Paused()
	if !paused {
		a.emitOutput(a.activeOrchID, "text", "\n[Nothing to continue — no paused run]\n")
		return
	}
	a.handlePrompt(ctx, autoContinuePrompt(a.runOutputDir))
}

// autoContinuePrompt is the synthetic "resume" turn used by both a manual
// continue and the auto-continue budget loop. outputDir, when set, restates
// the run's results directory so the save location survives compaction of the
// original prompt on long runs.
func autoContinuePrompt(outputDir string) *msg.MsgAgenticPrompt {
	return &msg.MsgAgenticPrompt{
		Prompt: withOutputDirNote(
			"Continue from where you stopped. Re-issue any tool call that was interrupted before completing the task.",
			outputDir),
	}
}

// withOutputDirNote appends the run's results-directory reminder to a synthetic
// prompt. Empty dir → prompt unchanged.
func withOutputDirNote(prompt, outputDir string) string {
	if outputDir == "" {
		return prompt
	}
	return prompt + "\nReminder: save all result files into this exact directory (not the cwd): " + outputDir
}

// handleSetRunBudget arms (or clears) the auto-continue budget for this pane's
// upcoming run. AutoContinue=false clears it. The leg allowance is derived from
// the total-iteration budget and the per-leg iteration cap: e.g. 300 total with
// a 50-cap → 6 legs → 5 automatic resumes.
func (a *LLMPromptExecutionActor) handleSetRunBudget(m *msg.MsgSetRunBudget) {
	// A persistent arm sets the ACTOR-level flag, which disarmRunBudget never
	// touches — this is what makes a fleet agent approval-free for its whole
	// life rather than for exactly one turn (the run-scoped flag below is
	// cleared on every terminal outcome, clean finishes included). Applied for
	// both the armed and the cleared branch, and symmetric: a persistent
	// AutoApprove=false explicitly revokes a sticky grant.
	if m.AutoApprovePersist {
		a.autoApproveAll = m.AutoApprove
		slog.Info("agentic: persistent auto-approve set", "pane", a.paneID, "autoApprove", m.AutoApprove)
	}
	if !m.AutoContinue {
		a.disarmRunBudget() // clears runAutoApprove; re-set below since it's independent
		a.runAutoApprove = m.AutoApprove
		slog.Info("agentic: run budget cleared", "pane", a.paneID, "autoApprove", m.AutoApprove)
		return
	}
	a.runAutoApprove = m.AutoApprove
	a.runOutputDir = m.OutputDir
	a.finalizerPrompt = m.FinalizerPrompt
	a.runModel = m.Model
	a.runEffort = m.Effort
	a.runFinalizerModel = m.FinalizerModel
	a.runFinalizerEffort = m.FinalizerEffort
	a.finalizerPhase = false
	a.runFinalizerMaxIter = m.FinalizerMaxIterations
	a.runFinalizerMaxDur = time.Duration(m.FinalizerMaxDurationMs) * time.Millisecond
	a.runFinalizerMaxTokens = m.FinalizerMaxContextTokens
	// Per-leg step cap: the recipe's step_interval, else the actor default.
	perLeg := m.StepInterval
	if perLeg <= 0 {
		perLeg = a.maxIterations
	}
	if perLeg <= 0 {
		perLeg = 20
	}
	a.runStepInterval = perLeg
	a.runMaxContextTokens = m.MaxContextTokens
	a.runTokensUsed = 0
	a.runCacheReadTokens, a.runCacheWriteTokens, a.runFreshInputTokens = 0, 0, 0
	a.runTokensUsedTotal = 0
	a.sessionGrounded = false
	a.runArmedAt = time.Now()
	total := m.MaxTotalIterations
	if total < perLeg {
		total = perLeg
	}
	legs := (total + perLeg - 1) / perLeg
	a.autoContinueArmed = true
	a.autoContinuesLeft = legs - 1
	if a.autoContinuesLeft < 0 {
		a.autoContinuesLeft = 0
	}
	a.autoContinueMaxDur = time.Duration(m.MaxDurationMs) * time.Millisecond
	if a.autoContinueMaxDur > 0 {
		a.autoContinueDeadline = time.Now().Add(a.autoContinueMaxDur)
	} else {
		a.autoContinueDeadline = time.Time{}
	}
	slog.Info("agentic: run budget armed", "pane", a.paneID,
		"stepInterval", perLeg, "autoContinues", a.autoContinuesLeft,
		"maxDuration", a.autoContinueMaxDur, "maxContextTokens", a.runMaxContextTokens)
}

// disarmRunBudget clears the auto-continue budget and finalizer state (clean
// finish, user cancel, or budget fully exhausted after the finalizer).
func (a *LLMPromptExecutionActor) disarmRunBudget() {
	a.runModel, a.runEffort = "", ""
	a.runFinalizerModel, a.runFinalizerEffort = "", ""

	a.autoContinueArmed = false
	a.autoContinuesLeft = 0
	a.autoContinueDeadline = time.Time{}
	a.autoContinueMaxDur = 0
	a.runStepInterval = 0
	a.runMaxContextTokens = 0
	a.runTokensUsed = 0
	// accounting (cache counters, runTokensUsedTotal) is deliberately PRESERVED
	// on disarm so the loop supervisor can sample it for the run report even
	// after a user cancel; it resets on the next arm.
	a.runArmedAt = time.Time{}
	a.finalizerPrompt = ""
	a.finalizerPhase = false
	a.runFinalizerMaxIter = 0
	a.runFinalizerMaxDur = 0
	a.runFinalizerMaxTokens = 0
	a.runAutoApprove = false
	a.runOutputDir = ""
}

// finalizer fallback budget: used when no reserved sub-budget was supplied (a
// fresh, small allowance so the closing leg can complete without looping).
const (
	finalizerMaxResumes = 1
	finalizerDeadline   = 5 * time.Minute
)

// armFinalizerBudget configures the auto-continue state for the finalizer leg
// from the reserved sub-budget (the budget_percent share), falling back to the
// built-in allowance for any unset field. Steps become a fresh leg allowance,
// duration a fresh deadline, and the token cap is the finalizer's context
// ceiling — the FULL budget, i.e. headroom above the task's context.
// legProvider returns the provider for the NEXT leg, applying the per-run
// model/effort seats (MsgSetRunBudget): the finalizer leg uses the finalizer
// seat with fallback to the run seat; main legs use the run seat. When no
// seat is set — or the provider cannot be overridden — the configured
// provider is returned unchanged.
func (a *LLMPromptExecutionActor) legProvider() provider.AgenticProvider {
	model, effort := a.runModel, a.runEffort
	if a.finalizerPhase {
		if a.runFinalizerModel != "" {
			model = a.runFinalizerModel
		}
		if a.runFinalizerEffort != "" {
			effort = a.runFinalizerEffort
		}
	}
	// SecretNAT: every return path wraps AFTER seat resolution, so a per-run
	// model/effort override can never hand the orchestrator an unwrapped
	// provider (secretnat.Wrap is identity when a.nat is nil, and the
	// decorator itself re-wraps on WithModelEffort as a second line of
	// defense).
	if model == "" && effort == "" {
		return secretnat.Wrap(a.prov, a.nat)
	}
	mo, ok := a.prov.(provider.ModelEffortOverridable)
	if !ok {
		return secretnat.Wrap(a.prov, a.nat)
	}
	slog.Info("agentic: leg model/effort override", "pane", a.paneID,
		"model", model, "effort", effort, "finalizer", a.finalizerPhase)
	return secretnat.Wrap(mo.WithModelEffort(model, effort), a.nat)
}

func (a *LLMPromptExecutionActor) armFinalizerBudget() {
	perLeg := a.runStepInterval
	if perLeg <= 0 {
		perLeg = a.maxIterations
	}
	if perLeg <= 0 {
		perLeg = 20
	}
	if a.runFinalizerMaxIter > 0 {
		legs := (a.runFinalizerMaxIter + perLeg - 1) / perLeg
		a.autoContinuesLeft = legs - 1
		if a.autoContinuesLeft < 0 {
			a.autoContinuesLeft = 0
		}
	} else {
		a.autoContinuesLeft = finalizerMaxResumes
	}
	dur := a.runFinalizerMaxDur
	if dur <= 0 {
		dur = finalizerDeadline
	}
	a.autoContinueMaxDur = dur
	a.autoContinueDeadline = time.Now().Add(dur)
	// Finalizer token budget: a fresh cumulative allowance (its reserved share).
	// Reset the accumulator so the finalizer's legs count from zero. 0 lifts the cap.
	a.runMaxContextTokens = a.runFinalizerMaxTokens
	a.runTokensUsed = 0
	// finalizer takeover: only the ENFORCEMENT counter resets (fresh reserved
	// share); cache counters + runTokensUsedTotal keep accumulating.
}

// groundingModeForLeg returns the grounding mode to thread into a new
// orchestrator leg. Pure (no actor state) so it's unit-testable: the
// finalizer/takeover leg gets "" (grounding off) — it continues an
// already-grounded conversation with a small reserved budget, and locking its
// write tools behind another grounding_report round would burn that budget on
// re-earning access instead of wrapping up.
func groundingModeForLeg(mode string, finalizerPhase bool) string {
	if finalizerPhase {
		return ""
	}
	return mode
}

// groundingModeForLegSticky additionally downgrades ENFORCED grounding to the
// advisory prompt once this run has already grounded. The gate flag is
// per-orchestrator, so every auto-continued leg used to re-close it — and once
// compaction dropped the old grounding_report turn, nothing in history said
// "already grounded" — forcing a full re-observe + grounding_report cycle at
// leg boundaries mid-task (reported as "grounding gate re-armed mid-task").
// The sticky flag lives on the ACTOR, not in the (compactable) history.
func groundingModeForLegSticky(mode string, finalizerPhase, sessionGrounded bool) string {
	m := groundingModeForLeg(mode, finalizerPhase)
	if m == GroundingEnforced && sessionGrounded {
		return GroundingPrompt
	}
	return m
}

// shouldRunFinalizer reports whether an exhausted run should execute its graceful
// finalizer. Pure (no actor state) so it's unit-testable: only when the budget
// was armed for THIS run (never a plain prompt), a finalizer is configured, we
// are not already finalizing, and the run stopped specifically on a budget cap —
// never on a user cancel or a question.
func shouldRunFinalizer(armed bool, stoppedReason, finalizerPrompt string, finalizerPhase bool) bool {
	if !armed || finalizerPhase || finalizerPrompt == "" {
		return false
	}
	return stoppedReason == msg.StoppedReasonMaxIterations ||
		stoppedReason == msg.StoppedReasonMaxDuration ||
		stoppedReason == msg.StoppedReasonMaxTokens
}

// autoContinueDecision reports whether a paused run should auto-resume. Pure
// (no actor state / clock) so the budget rules are unit-testable: resume only
// when armed, with resumes left, within the deadline, and paused specifically
// on a budget cap (iteration/time) — never on a user cancel or a question.
func autoContinueDecision(armed bool, left int, deadline time.Time, stoppedReason string, now time.Time) bool {
	if !armed || left <= 0 {
		return false
	}
	if stoppedReason != msg.StoppedReasonMaxIterations && stoppedReason != msg.StoppedReasonMaxDuration {
		return false
	}
	if !deadline.IsZero() && !now.Before(deadline) {
		return false
	}
	return true
}

// mergeDoneIntoSession applies a finished orchestrator's conversation to the
// session and reports whether the Done was authoritative (should proceed) or
// stale (should be ignored).
//
// The Done is authoritative iff its id matches the actor's activeOrchID; a
// mismatch is dropped. That single rule serves three flows correctly:
//
//   - Superseded run (the bug this fixes): a new prompt does NOT spawn a second
//     orchestrator — handlePrompt queues it and leaves activeOrchID pointing at
//     the in-flight run — so when that run's Done arrives (Interrupted) it still
//     MATCHES and its full tool work (browsing results, draft ids, edits) is
//     merged. The old code spawned a rival orchestrator, so the superseded run's
//     Done mismatched the new id and was dropped, and the follow-up prompt ran
//     against a near-empty transcript ("this is the very first message").
//   - Clean completion: activeOrchID matches → merge.
//   - Abandoned run (##hop fork): handleSessionMemoryReplace clears activeOrchID
//     to "", so the stopped orchestrator's late Done mismatches and is dropped
//     rather than clobbering the freshly forked memory.
//
// Kept as a pure helper (no actor.Context) so the merge invariant is unit-
// testable without a live actor system / NATS.
func mergeDoneIntoSession(session *SessionMemory, activeOrchID string, m *msg.MsgOrchestratorDone) bool {
	if m.OrchestratorID != activeOrchID {
		return false
	}
	if len(m.Conversation) > 0 {
		session.Replace(m.Conversation)
	} else if m.Summary != "" {
		session.Append(provider.ConversationTurn{
			Role:     "assistant",
			Content:  m.Summary,
			Category: provider.TurnCategoryAI,
		})
	}
	// Trim + heal the window (no orphaned tool_result at the head; no
	// unanswered tool_use at the tail).
	session.Bound()
	return true
}

func (a *LLMPromptExecutionActor) handleOrchestratorDone(ctx actor.Context, m *msg.MsgOrchestratorDone) {
	// Merge the finished run's conversation into the session (or ignore a truly
	// stale Done). See mergeDoneIntoSession for why the merge must happen even
	// for a superseded/cancelled run.
	if !mergeDoneIntoSession(a.session, a.activeOrchID, m) {
		slog.Info("agentic: ignoring unrecognized orchestrator done", "pane", a.paneID, "orch", m.OrchestratorID, "active", a.activeOrchID)
		return
	}

	// Accumulate this leg's token usage toward the run's cumulative token budget,
	// so a budget spanning several auto-continued legs is enforced across them.
	a.runTokensUsed += m.TokensUsed
	a.runTokensUsedTotal += m.TokensUsed
	// Prompt-cache accounting across legs (surfaced by MsgGetRunStatus).
	a.runCacheReadTokens += m.CacheReadTokens
	a.runCacheWriteTokens += m.CacheWriteTokens
	a.runFreshInputTokens += m.FreshInputTokens
	// Sticky groundedness: once any leg grounded, later legs of this run
	// don't re-close the enforced gate (survives compaction — flag, not
	// history).
	if !a.sessionGrounded && LastGroundingReport(a.session.Turns()) != nil {
		a.sessionGrounded = true
	}

	// Clean up the finished orchestrator.
	if a.activeOrch != nil {
		ctx.Stop(a.activeOrch)
		a.activeOrch = nil
	}
	a.activeOrchID = ""
	if a.cancelFn != nil {
		a.cancelFn()
		a.cancelFn = nil
	}

	// A prompt queued while this run was in flight now starts — with the run's
	// work already merged above, so it has full context.
	if a.pendingPrompt != nil {
		p := a.pendingPrompt
		a.pendingPrompt = nil
		a.session.ClearPause() // the queued prompt supersedes any interrupt checkpoint
		a.startPrompt(ctx, p)
		return
	}

	// No queued prompt: a genuine interrupt (user cancel, iteration/time cap,
	// awaiting-user-info) becomes a resumable checkpoint, persisted immediately
	// so it survives a daemon restart. A clean finish clears any pause.
	if m.Interrupted {
		// Auto-continue (##web auto long runs): when the run paused only because
		// it hit a per-leg budget cap, resume it automatically — the previous
		// leg's work is already merged above, so it picks up seamlessly. Bounded
		// by the leg allowance and the wall-clock deadline; when either is spent
		// it falls through to a normal pause for a manual continue.
		if autoContinueDecision(a.autoContinueArmed, a.autoContinuesLeft, a.autoContinueDeadline, m.StoppedReason, time.Now()) {
			a.autoContinuesLeft--
			slog.Info("agentic: auto-continue", "pane", a.paneID, "reason", m.StoppedReason, "left", a.autoContinuesLeft)
			a.emitOutput(m.OrchestratorID, "text", fmt.Sprintf("\n[auto-continuing — %d resume(s) left in budget]\n", a.autoContinuesLeft))
			a.session.ClearPause()
			a.startPrompt(ctx, autoContinuePrompt(a.runOutputDir))
			return
		}
		// Budget exhausted (hit the iteration/time/token cap without finishing): run
		// the graceful finalizer ONCE as a dedicated closing leg with its RESERVED
		// sub-budget (the budget_percent share), so it can complete (save what was
		// collected + summarize what's incomplete). It shares this run's
		// conversation, so it sees everything the browsing legs gathered.
		if shouldRunFinalizer(a.autoContinueArmed, m.StoppedReason, a.finalizerPrompt, a.finalizerPhase) {
			a.finalizerPhase = true
			a.autoContinueArmed = true
			a.armFinalizerBudget()
			slog.Info("agentic: running finalizer", "pane", a.paneID, "reason", m.StoppedReason,
				"finIter", a.runFinalizerMaxIter, "finDur", a.autoContinueMaxDur, "finTokens", a.runMaxContextTokens)
			a.emitOutput(m.OrchestratorID, "text", "\n[budget reached — running finalizer to wrap up gracefully]\n")
			a.session.ClearPause()
			a.startPrompt(ctx, &msg.MsgAgenticPrompt{Prompt: withOutputDirNote(a.finalizerPrompt, a.runOutputDir)})
			return
		}

		// Not auto-continuing: a user cancel or an exhausted budget (including
		// after the finalizer) disarms so a later manual prompt isn't unexpectedly
		// auto-resumed. (An awaiting-info pause keeps the budget armed: the user's
		// answer is part of the same task and should still auto-complete.)
		if m.StoppedReason == msg.StoppedReasonCancelled ||
			m.StoppedReason == msg.StoppedReasonMaxIterations ||
			m.StoppedReason == msg.StoppedReasonMaxDuration ||
			m.StoppedReason == msg.StoppedReasonMaxTokens {
			a.disarmRunBudget()
		}
		a.session.Pause(m.StoppedReason)
		a.session.Persist(true)
	} else {
		a.disarmRunBudget() // clean finish — target reached
		a.session.ClearPause()
		a.session.Persist(false)
	}
	// Terminal status (done/paused/error) is emitted by the Orchestrator
	// itself in its runLoop defer, ensuring NATS publish ordering.
}

func (a *LLMPromptExecutionActor) handleGetConversationHistory(_ actor.Context, m *msg.MsgGetConversationHistory, env *msg.RequestEnvelope) {
	lastN := m.LastN
	if lastN <= 0 {
		lastN = 20
	}
	if lastN > 100 {
		lastN = 100
	}

	conv := a.session.Turns()
	if len(conv) > lastN {
		conv = conv[len(conv)-lastN:]
	}

	turns := make([]msg.ConversationTurnInfo, 0, len(conv))
	for _, turn := range conv {
		info := msg.ConversationTurnInfo{
			Role:        turn.Role,
			Content:     turn.Content,
			ToolCallID:  turn.ToolCallID,
			IsError:     turn.IsError,
			Category:    provider.CategorizeTurn(turn),
			Origin:      turn.Origin,
			Summary:     turn.Summary,
			TimestampMs: turn.TimestampMs,
		}
		for _, tc := range turn.ToolCalls {
			info.ToolCalls = append(info.ToolCalls, msg.ToolCallInfo{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: tc.Input,
			})
		}
		turns = append(turns, info)
	}

	reply := &msg.MsgConversationHistoryReply{
		PaneID: a.paneID,
		Turns:  turns,
	}

	if env != nil {
		_ = env.Reply(reply)
	}
}

// persistConversation writes the current session state to the KV store,
// throttled by SessionMemory. Kept as a named wrapper for readability at
// call sites.
func (a *LLMPromptExecutionActor) persistConversation() {
	a.session.Persist(false)
}

// buildEffectiveSystemPrompt combines the base system prompt with the per-turn
// environment block (live working directory, git, layout) and memory context.
// The env block is rendered fresh each turn so it tracks the shell cwd after a
// `cd`; memory entries, when present, are appended after it.
func (a *LLMPromptExecutionActor) buildEffectiveSystemPrompt() string {
	sp := a.systemPrompt
	if a.envBlockProvider != nil {
		if env := a.envBlockProvider(); env != "" {
			sp = sp + "\n\n" + env
		}
	}
	if a.memoryState != nil && len(a.memoryState.Entries) > 0 {
		sp = sp + "\n" + msg.FormatMemoryForPrompt(a.memoryState)
	}
	return sp
}

func (a *LLMPromptExecutionActor) emitOutput(orchID, outputType, content string) {
	// Emit to the agentic output subject (used by Chrome extension and TUI).
	_ = a.pub.Send(a.outputSubject, &msg.MsgAgenticOutput{
		OrchestratorID: orchID,
		Type:           outputType,
		Content:        content,
	})

	// Emit via the unified ConversationMessage format to pane output topics.
	cm := &msg.ConversationMessage{
		TurnID:           orchID, // Use orchestrator ID as turn ID
		TurnType:         msg.TurnAnswer,
		ConversationType: msg.ConvAI,
		InputType:        msg.InputPrompt,
		MessageSource:    msg.SourceAI,
		Content:          content,
		TimestampMs:      msg.NowMs(),
		SubjectToShare:   true,
	}

	if a.pipelineOutputSubject != "" {
		_ = a.pub.Send(a.pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: content})
	} else if a.chatOutputPaneID != "" {
		cm.ConversationType = msg.ConvChat
		cm.Role = "assistant"
		_ = a.pub.SendConversation(a.chatOutputPaneID, cm)
	} else {
		_ = a.pub.SendConversation(a.paneID, cm)
	}
}

func (a *LLMPromptExecutionActor) emitStatus(orchID, phase string, iteration, maxIter int) {
	_ = a.pub.Send(a.statusSubject, &msg.MsgAgenticStatus{
		OrchestratorID: orchID,
		Phase:          phase,
		Iteration:      iteration,
		MaxIterations:  maxIter,
	})
	// Also update pane status
	paneStatus := msg.T("pane", a.paneID, "status")
	statusText := fmt.Sprintf("[agentic] %s", phase)
	if iteration > 0 {
		statusText = fmt.Sprintf("[agentic] %s | iter %d/%d", phase, iteration, maxIter)
	}
	_ = a.pub.Send(paneStatus, &msg.MsgPaneStatusUpdate{Status: statusText})
}

// Conversation returns the current conversation history (for persistence).
func (a *LLMPromptExecutionActor) Conversation() []provider.ConversationTurn {
	return a.session.Turns()
}

// Session exposes the session memory (turns + pause checkpoint). Hosts use it
// for diagnostics; mutation goes through actor messages only.
func (a *LLMPromptExecutionActor) Session() *SessionMemory {
	return a.session
}

// SetAutoApproval grants automatic approval for a tool+context combination.
func (a *LLMPromptExecutionActor) SetAutoApproval(key string) {
	a.autoApproved.Approve(key)
}

// SetAutoApproveAll enables headless auto-approval for every orchestrator this
// actor spawns: all tool calls run without an approval prompt. Used for
// humanoids/agents that have no human approver.
func (a *LLMPromptExecutionActor) SetAutoApproveAll(b bool) {
	a.autoApproveAll = b
}

// SetGroundingMode selects the grounding protocol (GroundingOff /
// GroundingPrompt / GroundingEnforced) threaded into every orchestrator this
// actor spawns. See grounding.go.
func (a *LLMPromptExecutionActor) SetGroundingMode(mode string) {
	a.groundingMode = mode
	a.groundingModeDefault = mode
}

// handleSetGroundingMode services a runtime ##grounding override. The change
// applies from the NEXT prompt — an in-flight orchestrator keeps the mode it
// captured at spawn. Invalid values are ignored with a log line (the CLI
// validates before sending, so this is belt-and-suspenders).
func (a *LLMPromptExecutionActor) handleSetGroundingMode(m *msg.MsgSetGroundingMode) {
	// Mode "" clears the override: revert to the host default and forget the
	// persisted value (##grounding reset).
	if m.Mode == "" {
		a.groundingMode = a.groundingModeDefault
		a.session.SetGroundingOverride("")
		a.session.Persist(true)
		slog.Info("agentic: grounding override cleared", "pane", a.paneID,
			"mode", a.groundingMode)
		return
	}
	if !ValidGroundingMode(m.Mode) {
		slog.Warn("agentic: invalid grounding mode ignored", "pane", a.paneID, "mode", m.Mode)
		return
	}
	a.groundingMode = m.Mode
	// Persist the override with the session so it survives daemon restarts.
	// Deliberately NOT transferred by a ##hop fork (session replace only
	// swaps turns; each pane keeps its own settings).
	a.session.SetGroundingOverride(m.Mode)
	a.session.Persist(true)
	slog.Info("agentic: grounding mode overridden", "pane", a.paneID,
		"mode", m.Mode, "default", a.groundingModeDefault)
}

// handleGetGroundingState replies with the grounding state for the
// ##grounding status/report display: effective + default mode, whether a run
// is in flight, and the most recent grounding_report recorded in session
// memory.
func (a *LLMPromptExecutionActor) handleGetGroundingState(env *msg.RequestEnvelope) {
	mode := a.groundingMode
	if mode == "" {
		mode = GroundingOff
	}
	def := a.groundingModeDefault
	if def == "" {
		def = GroundingOff
	}
	_ = env.Reply(&msg.MsgGroundingStateReply{
		PaneID:      a.paneID,
		Mode:        mode,
		DefaultMode: def,
		Overridden:  mode != def,
		RunActive:   a.activeOrch != nil,
		LastReport:  LastGroundingReport(a.session.Turns()),
	})
}

// InboxSubject returns the NATS subject this actor listens on.
func (a *LLMPromptExecutionActor) InboxSubject() string {
	return a.inboxSubject
}

// maxConversationTurns caps the retained conversation window. Older turns are
// dropped from the front; context-window compaction (orchestrator side) handles
// summarizing what falls off across longer sessions.
// maxConversationTurns is the TARGET turn count boundConversation trims down
// to; maxConversationTurnsHard is the threshold that TRIGGERS the trim.
//
// The gap is deliberate hysteresis: trimming on every excess turn made the
// window SLIDE at every leg boundary (Bound() runs on each merge), changing
// the transcript head each time — which invalidated Anthropic's incremental
// prompt cache and re-wrote the whole conversation at full budget weight
// once per leg (~5x per ##auto pass; observed burning a 571k-token pass in
// minutes on a typing-heavy browser run). With hysteresis the head moves
// only once per ~(hard-target) new turns, and the token-based orchestrator
// compaction (which pins the original task and summarizes) usually fires
// first — this is the rare backstop, not the primary window manager.
const (
	maxConversationTurns     = 200
	maxConversationTurnsHard = 300
)

// boundConversation keeps the most recent maxConversationTurns turns and then
// heals the head so the window never begins with an orphaned tool_result.
//
// A plain tail cut (conversation[len-N:]) can land between an assistant
// tool_use turn and its "tool" result turn, leaving a leading tool_result with
// no preceding tool_use — which the Messages API rejects with HTTP 400. It also
// guards conversations restored from KV that were persisted in that broken
// state. provider.HealConversationHead advances past any such dangling head to
// the first genuine user turn.
func boundConversation(turns []provider.ConversationTurn) []provider.ConversationTurn {
	if len(turns) > maxConversationTurnsHard {
		turns = turns[len(turns)-maxConversationTurns:]
	}
	return provider.HealConversationHead(turns)
}

// formatTimestamp returns a compact timestamp for logging.
func formatTimestamp() string {
	return time.Now().Format("15:04:05")
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// indentBlock indents every line of text by prefix.
func indentBlock(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
