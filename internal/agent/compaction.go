package agent

import "google.golang.org/adk/v2/model"

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

// usable is the input budget: context window minus output reserve.
func usable(contextWindow int) int {
	if u := contextWindow - compactionBuffer; u > 0 {
		return u
	}
	return 0
}
