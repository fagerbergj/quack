package agent

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
)

// fakeLLM is a minimal model.LLM stub - ResolveSummarizer only needs identity,
// never calls GenerateContent.
type fakeLLM struct{ text string }

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

// ResolveSummarizer must prefer the active run/node's own worker model - it
// is already resident, so reusing it for compaction is swap-free by
// construction - and fall back to the configured model only when no active model is available (e.g. a standalone compaction).
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
