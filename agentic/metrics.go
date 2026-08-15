// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"time"

	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// Follow-up item 3 — observability hooks for the agentic system.
//
// MetricsSink is the small interface the orchestrator and Setup call into
// when significant events happen: a tool finishes, an LLM call returns,
// compaction runs, an approval decision lands. Implementations are
// expected to be cheap (return quickly; do real work in the background).
//
// rysh-cli ships a Prometheus implementation under rysh-cli/internal/metrics/;
// every other binary / test uses the NoopMetricsSink default so nothing
// breaks when metrics aren't wired up.
type MetricsSink interface {
	// RecordTool fires after a tool's Execute returns (success or error).
	// dur is wall-clock elapsed; out is the result (may be nil on a Go
	// error before the tool produced a ToolOutput); err is the Go-level
	// error if any.
	RecordTool(name string, dur time.Duration, out *tools.ToolOutput, err error)

	// RecordLLMCall fires after each provider.CompleteWithTools[Stream]
	// returns. usage carries token accounting; cache hit ratio is
	// derivable from CacheReadInputTokens / TotalInputTokens.
	RecordLLMCall(model string, dur time.Duration, usage provider.Usage)

	// RecordCompaction fires after maybeCompact summarises and drops old
	// turns. droppedTurns is the number of original turns replaced.
	RecordCompaction(droppedTurns int, dur time.Duration)

	// RecordApproval fires for every approval decision (yes / yes_always
	// / no / no_with_reason / timeout).
	RecordApproval(toolName, decision string, dur time.Duration)
}

// NoopMetricsSink is the default; safe to use everywhere. Production
// rysh-cli replaces it with a Prometheus-backed implementation at startup.
type NoopMetricsSink struct{}

func (NoopMetricsSink) RecordTool(string, time.Duration, *tools.ToolOutput, error) {}
func (NoopMetricsSink) RecordLLMCall(string, time.Duration, provider.Usage)        {}
func (NoopMetricsSink) RecordCompaction(int, time.Duration)                        {}
func (NoopMetricsSink) RecordApproval(string, string, time.Duration)               {}

// SetMetricsSink installs a metrics sink on this orchestrator. Pass nil
// to fall back to NoopMetricsSink. Follow-up item 3.
func (o *OrchestratorActor) SetMetricsSink(s MetricsSink) {
	if s == nil {
		s = NoopMetricsSink{}
	}
	o.metrics = s
}

// SetCwdResolver installs the working-directory resolver. When set, runLoop
// evaluates it once at the start of the run and re-points working-directory
// aware tools at the returned path (the pane's live shell cwd).
func (o *OrchestratorActor) SetCwdResolver(fn func() string) {
	o.cwdResolver = fn
}
