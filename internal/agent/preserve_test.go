package agent

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// The preserved (verbatim, recent) tail must be a FRACTION OF THE BUDGET, not a constant.
//
// THE ROOT CAUSE (code-mode dogfood, 2026-07-13). A code-implementer ran 25 minutes, made
// 98 tool calls, and wrote nothing. It read internal/tools/registry.go TEN TIMES. Its
// session had reached ~166k tokens against a 65,536-token window, so compaction ran every
// turn — and compaction preserved only:
//
//	min(max(budget/4, 2000), 8000) = 8_000 tokens
//
// out of a 45,536-token budget. Everything else was summarised away. A Go source file is
// 6-7k tokens, so the tail could hold exactly ONE FILE: every new read evicted the
// previous one. The model could never hold two files at once, so it could never write
// code that used both — and each time it re-read what we had just deleted, we counted
// that against it as churn.
//
// 8_000 is opencode's MAX_PRESERVE_RECENT_TOKENS, ported without its premise: opencode
// runs 200k+ context windows, where 8k of recent turns is a rounding error.
func TestPreserveScalesWithTheBudgetInsteadOfCollapsingTo8k(t *testing.T) {
	const window = 65_536
	budget := usable(window) // 45,536
	const head = 500         // a normal node task
	const ratio = defaultCalibrationRatio
	const overhead = 6_000 // the provider's fixed system+tools overhead

	// A normal turn: nothing measured over budget, a small task, a few big reads.
	contents := []*genai.Content{textContent(genai.RoleUser, strings.Repeat("t", head*charsPerToken))}
	contents = append(contents, textContent(genai.RoleModel, strings.Repeat("f", 20_000*charsPerToken)))
	got := int(float64(preserveFor(budget, contents, ratio, overhead, 0)) * ratio) // back to real tokens

	// The old model's answer, for contrast — it must not be reachable.
	old := min(max(budget/4, minPreserveTokens), 8_000)
	if got <= old {
		t.Fatalf("preserve = %d, no better than the old fixed cap (%d) — the model still cannot hold two files", got, old)
	}

	// A source file is ~6-7k tokens. The tail must hold SEVERAL, or a coding agent
	// cannot write code that spans them.
	const sourceFile = 7_000
	if got < 3*sourceFile {
		t.Errorf("preserve = %d tokens: that is fewer than 3 source files (%d). An implementer must be able to hold "+
			"the file it is editing, the file it is calling, and its test at the same time", got, 3*sourceFile)
	}

	// ...and it must still leave room for the task and the summary that ride on it.
	if got >= budget {
		t.Errorf("preserve = %d >= budget %d: nothing left for the task or the anchored summary", got, budget)
	}
}

// A pathological head (an enormous task/revise prompt) must not drive preserve negative
// or to zero — truncateHeadToFit owns that case, and the tail keeps a usable floor.
func TestPreserveNeverCollapsesOnAnOversizedHead(t *testing.T) {
	budget := usable(65_536)
	huge := []*genai.Content{textContent(genai.RoleUser, strings.Repeat("t", 1_000_000*charsPerToken))}
	got := preserveFor(budget, huge, defaultCalibrationRatio, 6_000, 0)
	if got < minPreserveTokens {
		t.Fatalf("preserve = %d, below the %d floor — an oversized task must not leave the model with no history",
			got, minPreserveTokens)
	}
}

// A tiny window still behaves: preserve stays positive and under budget.
func TestPreserveOnASmallWindow(t *testing.T) {
	budget := usable(32_768)
	small := []*genai.Content{textContent(genai.RoleUser, "task"), textContent(genai.RoleModel, strings.Repeat("x", 40_000))}
	got := preserveFor(budget, small, defaultCalibrationRatio, 2_000, 0)
	if got <= 0 || got >= budget {
		t.Fatalf("preserve = %d for budget %d", got, budget)
	}
}
