package agent

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// Compaction summarizes older turns of a session, folding them into a
// durable event via adk/v2's native runner-level compaction (see a2a.go's
// nativeCompactionConfig). Raw events are never deleted.
const (
	charsPerToken = 4

	compactionBuffer          = 20_000
	defaultEventRetentionSize = 20
)

type Compaction struct {
	Summarizer         model.LLM
	ContextWindow      int
	Enabled            bool
	TokenThreshold     int
	EventRetentionSize int
	// CompactionInterval is adk's regular-cadence trigger (in invocations),
	// on top of TokenThreshold's absolute limit. 0 disables the cadence trigger.
	CompactionInterval int
	// OverlapSize is how many already-windowed raw events carry into the next
	// summarization pass, so a fact split across the cut isn't lost. 0 = default.
	OverlapSize int
}

// ResolveSummarizer prefers the active worker model for compaction (swap-free), falling back to the configured one.
func ResolveSummarizer(active, fallback model.LLM) model.LLM {
	if active != nil {
		return active
	}
	return fallback
}

// usable is the input budget: context window minus output reserve, capped at
// contextWindow/8 like internal/dag's budgetOutputReserve, so the two agree.
func usable(contextWindow int) int {
	reserve := compactionBuffer
	if ceil := contextWindow / 8; ceil < reserve {
		reserve = ceil
	}
	if u := contextWindow - reserve; u > 0 {
		return u
	}
	return 0
}

// emitCompaction publishes ev's compaction record to the chat's hub and opens a paired otel span; no-op for a non-compaction event or a nil sink
// (compaction disabled, or a call site - e.g. tests - with no hub). ctx must be the worker's own per-request context (from compactionSessions'
// AppendEvent): otelhttp's server handler already parented it from the caller's traceparent, so the span lands under the dispatching round's trace without the ledger.Coords.SpanContext workaround the old in-process callback needed.
func emitCompaction(ctx context.Context, sink func(stream.SSEEvent), nodeID string, ev *session.Event) {
	if sink == nil || ev == nil || ev.Actions.Compaction == nil {
		return
	}
	c := ev.Actions.Compaction
	var in, out int32
	if u := ev.LLMResponse.UsageMetadata; u != nil {
		in, out = u.PromptTokenCount, u.CandidatesTokenCount+u.ThoughtsTokenCount
	}
	// ev.Branch is "<name>@<runID>" (workflow.WithUseSubBranch); RunIDFromBranch
	// is the same extraction dag.segRun uses, so this matches the round's
	// agent_start run id exactly instead of adk's own (unrelated) invocation id.
	sink(stream.Compaction(nodeID, stream.RunIDFromBranch(ev.Branch), c.StartTimestamp, c.EndTimestamp, in, out))

	_, span := otelobs.Start(ctx, "compaction", attribute.String("node_id", nodeID))
	if in > 0 {
		span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", int(in)))
	}
	if out > 0 {
		span.SetAttributes(attribute.Int("gen_ai.usage.output_tokens", int(out)))
	}
	span.End()
}
