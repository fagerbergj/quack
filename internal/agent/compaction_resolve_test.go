package agent

import (
	"testing"
)

// ResolveSummarizer prefers the already-resident worker model (swap-free), falling back to the configured one.
func TestResolveSummarizerPrefersActiveModel(t *testing.T) {
	active, fallback := &fakeLLM{}, &fakeLLM{}

	if got := ResolveSummarizer(active, fallback); got != active {
		t.Fatalf("ResolveSummarizer with both set = %v, want active model", got)
	}
}

func TestResolveSummarizerFallsBackWhenNoActiveModel(t *testing.T) {
	fallback := &fakeLLM{}
	if got := ResolveSummarizer(nil, fallback); got != fallback {
		t.Fatalf("ResolveSummarizer with no active model = %v, want fallback", got)
	}
}

func TestResolveSummarizerNilWhenNeitherSet(t *testing.T) {
	if got := ResolveSummarizer(nil, nil); got != nil {
		t.Fatalf("ResolveSummarizer with neither set = %v, want nil", got)
	}
}
