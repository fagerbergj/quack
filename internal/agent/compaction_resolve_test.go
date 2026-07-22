package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// ResolveSummarizer must prefer the active run/node's own worker model - that
// model is already resident, so reusing it for compaction is swap-free by
// construction - and fall back to the configured model only when no active
// model is available (e.g. a standalone compaction).
func TestResolveSummarizerPrefersActiveModel(t *testing.T) {
	active := &fakeLLM{text: "active"}
	fallback := &fakeLLM{text: "fallback"}

	if got := ResolveSummarizer(active, fallback); got != active {
		t.Fatalf("ResolveSummarizer with both set = %v, want active model", got)
	}
}

func TestResolveSummarizerFallsBackWhenNoActiveModel(t *testing.T) {
	fallback := &fakeLLM{text: "fallback"}

	if got := ResolveSummarizer(nil, fallback); got != fallback {
		t.Fatalf("ResolveSummarizer with no active model = %v, want fallback", got)
	}
}

func TestResolveSummarizerNilWhenNeitherSet(t *testing.T) {
	if got := ResolveSummarizer(nil, nil); got != nil {
		t.Fatalf("ResolveSummarizer with neither set = %v, want nil", got)
	}
}

// End-to-end: compaction actually invokes the ACTIVE model, not a distinct
// configured one, when both are wired into Compaction via ResolveSummarizer.
func TestCompactionUsesActiveModelNotFixedConfigModel(t *testing.T) {
	active := &fakeLLM{text: "## Goal\n- from active model"}
	fixedConfig := &fakeLLM{text: "## Goal\n- from fixed config model"}

	task := textContent(genai.RoleUser, "the self-contained task")
	contents := []*genai.Content{task}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}

	comp := Compaction{
		Summarizer:    ResolveSummarizer(active, fixedConfig),
		ContextWindow: budgetWindow(contents, defaultEventRetentionSize),
		Enabled:       true,
	}
	cb := compactionCallback(comp)

	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}

	if active.calls != 1 {
		t.Fatalf("active model called %d times; want 1", active.calls)
	}
	if fixedConfig.calls != 0 {
		t.Fatalf("fixed config model called %d times; want 0 (active model should be used)", fixedConfig.calls)
	}
	idx := sentinelIndex(req.Contents)
	if idx < 0 {
		t.Fatalf("no durable summary content in the request")
	}
	if got := req.Contents[idx].Parts[0].Text; !strings.Contains(got, "from active model") {
		t.Fatalf("summary did not come from the active model: %q", got)
	}
}
