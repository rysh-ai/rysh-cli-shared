package agentic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// SubAgentToolName is the tool name the model invokes to delegate a focused
// task to a child orchestrator. The orchestrator intercepts this name in
// executeTool and routes it through handleSubAgent rather than the registered
// ToolExecutor (whose Execute is a defensive no-op).
const SubAgentToolName = "sub_agent"

// MaxSubAgentDepth caps recursive sub-agent spawning. The top-level
// orchestrator runs at depth 0; the first sub-agent runs at depth 1; a child
// of that sub-agent would run at depth 2 and so on. Attempts to spawn beyond
// MaxSubAgentDepth return a structured error so the parent can recover.
const MaxSubAgentDepth = 2

// DefaultSubAgentMaxIterations bounds the child orchestrator's agentic loop
// independently of the parent's budget. Sub-tasks are intended to be focused
// and short; a tighter cap protects against runaway children.
const DefaultSubAgentMaxIterations = 25

// DefaultSubAgentTimeout is the wall-clock upper bound a parent will wait for
// a child orchestrator to report MsgOrchestratorDone. The parent's own
// cancellation propagates through the shared o.ctx as well.
const DefaultSubAgentTimeout = 15 * time.Minute

// DefaultSubAgentSystemPrompt is used when a sub_agent tool call doesn't
// supply its own `system_prompt`. It is a VARIABLE (not const) so that the
// hosting binary — rysh-cli — can replace its content at startup from the
// embedded rysh-cli-agent-prompts/system_sub_agent.md file. The fallback below
// keeps the package self-contained for tests and library callers.
var DefaultSubAgentSystemPrompt = `You are a focused sub-agent working on a single delegated task.

Operating constraints:
- Stay focused on the task below. Do not expand scope.
- Use the available tools to gather information and make changes as needed.
- Your transcript is NOT returned to the parent agent — only your final summary.
- End with a concise summary of what you did, what you found, and any caveats.

Produce a useful final answer; the parent will rely on your summary alone.`

// DefaultCompactionSummarizePrompt is the prompt body used by the compaction
// path when summarising dropped conversation turns. The token `{{transcript}}`
// is substituted with the rendered transcript at call time. As with the sub-
// agent prompt above this is a VARIABLE so rysh-cli can replace it from the
// embedded rysh-cli-agent-prompts/system_compaction_summarize.md file.
var DefaultCompactionSummarizePrompt = `Summarise the following assistant/user/tool conversation transcript into a concise set of bullet points capturing: the user's goals, key decisions, files changed, important findings, and any unresolved tasks. Preserve concrete identifiers (file paths, names, IDs). Be terse.

Transcript:
{{transcript}}`

// SubAgentParams mirrors the JSON schema exposed by the sub_agent tool. It is
// declared in the shared package so the orchestrator can parse the model's
// tool input directly without a runtime dependency on rysh-cli.
type SubAgentParams struct {
	Task         string   `json:"task"`
	Context      string   `json:"context,omitempty"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// ParseSubAgentParams decodes a sub_agent tool-call payload and validates
// minimum required fields.
func ParseSubAgentParams(raw json.RawMessage) (*SubAgentParams, error) {
	var p SubAgentParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid sub_agent params: %w", err)
		}
	}
	p.Task = strings.TrimSpace(p.Task)
	if p.Task == "" {
		return nil, fmt.Errorf("sub_agent: 'task' is required")
	}
	return &p, nil
}

// BuildSubAgentRegistry returns a new tools.ToolRegistry containing only the
// tools the sub-agent is allowed to use. If allowedTools is empty, the full
// parent registry is cloned (so the child inherits everything except the
// hard-coded depth cap on nested sub_agent calls).
//
// Unknown tool names in allowedTools are silently skipped — surfacing them as
// errors would force the model to know the exact registry contents, which it
// doesn't.
func BuildSubAgentRegistry(parent *tools.ToolRegistry, allowedTools []string) *tools.ToolRegistry {
	if parent == nil {
		return tools.NewToolRegistry()
	}
	if len(allowedTools) == 0 {
		return parent.Clone()
	}
	child := tools.NewToolRegistry()
	for _, name := range allowedTools {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if executor, ok := parent.Get(name); ok {
			child.Register(name, executor)
		}
	}
	return child
}

// subAgentSpawnReq is an internal self-message: the runLoop goroutine sends it
// to the actor mailbox so the actual ctx.Spawn happens inside Receive (the
// only context where proto.actor guarantees safe child creation).
//
// The goroutine then blocks on spawnedCh (for spawn success/failure) and
// doneCh (for the child's MsgOrchestratorDone routed via Receive).
type subAgentSpawnReq struct {
	childOrchID  string
	conversation []provider.ConversationTurn
	systemPrompt string
	maxIter      int
	childDepth   int
	registry     *tools.ToolRegistry

	spawnedCh chan *actor.PID
	spawnErr  chan error
	doneCh    chan *msg.MsgOrchestratorDone
}

// newSubAgentSpawnReq is a small constructor that pre-allocates the buffered
// channels at the right sizes (1 for spawn signals, 1 for done). Keeping the
// buffer at 1 means the Receive handler never blocks on send even if the
// goroutine has timed out / been cancelled.
func newSubAgentSpawnReq(orchID string, conv []provider.ConversationTurn, systemPrompt string, maxIter, depth int, registry *tools.ToolRegistry) *subAgentSpawnReq {
	return &subAgentSpawnReq{
		childOrchID:  orchID,
		conversation: conv,
		systemPrompt: systemPrompt,
		maxIter:      maxIter,
		childDepth:   depth,
		registry:     registry,
		spawnedCh:    make(chan *actor.PID, 1),
		spawnErr:     make(chan error, 1),
		doneCh:       make(chan *msg.MsgOrchestratorDone, 1),
	}
}

// FormatSubAgentSummary renders a child orchestrator's MsgOrchestratorDone as
// the human-readable Content of a tools.ToolOutput. Only the final summary
// (plus terse counts of files-changed and errors) is included — by design,
// the child's tool transcript is not surfaced to the parent.
func FormatSubAgentSummary(done *msg.MsgOrchestratorDone) string {
	if done == nil {
		return "Sub-agent returned no result."
	}
	var sb strings.Builder
	if done.Success {
		sb.WriteString("Sub-agent completed successfully.\n\n")
	} else {
		sb.WriteString("Sub-agent reported failure.\n\n")
	}
	if strings.TrimSpace(done.Summary) != "" {
		sb.WriteString("Summary:\n")
		sb.WriteString(strings.TrimSpace(done.Summary))
		sb.WriteString("\n")
	}
	// Pull the last assistant turn from the child's conversation as the answer
	// body — this is the most informative single piece for the parent.
	if last := lastAssistantText(done.Conversation); last != "" {
		sb.WriteString("\nFinal answer:\n")
		sb.WriteString(last)
		sb.WriteString("\n")
	}
	if len(done.FilesChanged) > 0 {
		sb.WriteString(fmt.Sprintf("\nFiles changed: %s\n", strings.Join(done.FilesChanged, ", ")))
	}
	if len(done.Errors) > 0 {
		sb.WriteString(fmt.Sprintf("\nErrors: %s\n", strings.Join(done.Errors, "; ")))
	}
	return sb.String()
}

// lastAssistantText returns the text of the most recent assistant turn that
// carries actual content (skipping tool-call-only turns).
func lastAssistantText(conv []provider.ConversationTurn) string {
	for i := len(conv) - 1; i >= 0; i-- {
		t := conv[i]
		if t.Role == "assistant" && strings.TrimSpace(t.Content) != "" {
			return strings.TrimSpace(t.Content)
		}
	}
	return ""
}

// BuildSubAgentSeedConversation constructs the single-user-turn conversation
// the child orchestrator starts with. Task and optional context are joined
// with a labelled separator so the child can distinguish them.
func BuildSubAgentSeedConversation(p *SubAgentParams) []provider.ConversationTurn {
	var sb strings.Builder
	sb.WriteString(p.Task)
	if strings.TrimSpace(p.Context) != "" {
		sb.WriteString("\n\nAdditional context:\n")
		sb.WriteString(strings.TrimSpace(p.Context))
	}
	return []provider.ConversationTurn{
		{Role: "user", Content: sb.String()},
	}
}

// NewSubAgentOrchID returns a UUID for a child orchestrator. Centralised so
// tests can swap it out if needed.
var NewSubAgentOrchID = func() string { return uuid.New().String() }
