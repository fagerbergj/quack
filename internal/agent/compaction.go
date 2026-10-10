package agent

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// Compaction configures adk's native runner-level compaction (NativeCompactionConfig); raw events are never deleted.
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
	// CompactionInterval is adk's cadence trigger in invocations, on top of TokenThreshold; 0 disables it.
	CompactionInterval int
	// OverlapSize carries already-windowed events into the next pass so a fact split across the cut survives; 0 = default.
	OverlapSize int
	// Prompts resolves the summarizer prompt once per node build; nil resolves the shipped files.
	Prompts *artifactsrc.Resolver
	// Meter lets a compaction collapse stale fetch/read results before paying for a summary (collapse.go).
	Meter *PromptMeter
}

// ResolveSummarizer prefers the active worker model for compaction (swap-free), falling back to the configured one.
func ResolveSummarizer(active, fallback model.LLM) model.LLM {
	if active != nil {
		return active
	}
	return fallback
}

// usable is the context window minus an output reserve capped at contextWindow/4, matching dag's budgetOutputReserve.
func usable(contextWindow int) int {
	reserve := compactionBuffer
	if ceil := contextWindow / 4; ceil < reserve {
		reserve = ceil
	}
	if u := contextWindow - reserve; u > 0 {
		return u
	}
	return 0
}

// emitCompaction publishes ev's compaction record and a paired otel span; no-op for other events or a nil sink.
// ctx must be the worker's per-request context, which otelhttp parented under the dispatching round's trace.
func emitCompaction(ctx context.Context, sink func(stream.SSEEvent), nodeID string, ev *session.Event) {
	if sink == nil || ev == nil || ev.Actions.Compaction == nil {
		return
	}
	c := ev.Actions.Compaction
	var in, out int32
	if u := ev.LLMResponse.UsageMetadata; u != nil {
		in, out = u.PromptTokenCount, u.CandidatesTokenCount+u.ThoughtsTokenCount
	}
	// ev.Branch is "<name>@<runID>"; this matches the round's agent_start run id, not adk's invocation id.
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
