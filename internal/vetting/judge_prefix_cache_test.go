// judge_prefix_cache_test.go: regression test for finding 4 of the
// agent-efficiency audit (stable-first section ordering in buildJudgePrompt - the answer being judged trails last instead of leading). See
// revise_prefix_cache_test.go for why: a byte stable across rounds and ahead of the first changing byte is a prefill saved under prod's shared vLLM prefix cache.
package vetting

import (
	"strings"
	"testing"
)

// TestJudgePromptSharedPrefixAcrossRounds pins finding 4: on the same node,
// round 2 (a different answer, same everything else) must share at least 85%
// of round 1's bytes as a common prefix - the reorder measured 91.7% on the audit's synthetic task, above the pre-fix 7.8%.
func TestJudgePromptSharedPrefixAcrossRounds(t *testing.T) {
	task := "Review pull request #1304 and post inline findings."
	question := questionContent("Review PR #1304 in fagerbergj/quack")
	rubric := strings.Repeat("criterion: grounding - every claim about this repo must be verified by reading it.\n", 40)
	constitution := strings.Repeat("Be precise. Do not fabricate.\n", 10)
	diff := "Changed files under review (diff of the clone):\n" + synthReviewTask()
	act := workerActivity{workspace: []wsOp{{tool: "read_file", detail: "read_file internal/recordstore/recordstore.go"}, {tool: "grep", detail: "grep lockFor"}}}
	ans1 := strings.Repeat("Round 1 answer: the per-id lock is correct.\n", 40)
	ans2 := strings.Repeat("Round 2 answer: the per-id lock is correct and the map is guarded.\n", 40)

	upstream := "--- from \"explore-1\" ---\nThe bug is in internal/dag/graph.go:262, upstreamFromInput.\n\n"

	p1 := buildJudgePrompt(constitution, rubric, task, upstream, question, ans1, diff, act, "")
	p2 := buildJudgePrompt(constitution, rubric, task, upstream, question, ans2, diff, act, "")

	shared := commonPrefixLen(p1, p2)
	frac := float64(shared) / float64(len(p2))
	t.Logf("judge prompt len r1=%d r2=%d; shared prefix=%d bytes (%.1f%%)", len(p1), len(p2), shared, 100*frac)
	const minFraction = 0.85
	if frac < minFraction {
		t.Fatalf("judge prompt round1->round2 shared prefix = %.1f%% of round 2, want >= %.0f%% - "+
			"a section that changes every round has moved ahead of the answer, killing the prefix cache",
			100*frac, 100*minFraction)
	}
}

// TestJudgePromptUpstreamSection proves the upstream-output section appears
// only when the node has upstream input, and sits ahead of the answer.
func TestJudgePromptUpstreamSection(t *testing.T) {
	task := "Fix the bug at the file and line identified in the previous exploration task."
	question := questionContent("Fix the reported bug")
	upstream := "--- from \"explore-1\" ---\nThe bug is in internal/dag/graph.go:262, upstreamFromInput.\n\n"

	withUpstream := buildJudgePrompt("", "rubric", task, upstream, question, "the fix", "", workerActivity{}, "")
	if !strings.Contains(withUpstream, "OUTPUT FROM UPSTREAM NODES") || !strings.Contains(withUpstream, "graph.go:262") {
		t.Fatalf("judge prompt with upstream output missing the upstream section:\n%s", withUpstream)
	}
	if idx, ans := strings.Index(withUpstream, "graph.go:262"), strings.Index(withUpstream, "the fix"); idx < 0 || ans < idx {
		t.Fatalf("upstream section must precede the answer being judged")
	}

	withoutUpstream := buildJudgePrompt("", "rubric", task, "", question, "the fix", "", workerActivity{}, "")
	if strings.Contains(withoutUpstream, "OUTPUT FROM UPSTREAM NODES") {
		t.Fatalf("judge prompt with no upstream input must not carry the upstream section:\n%s", withoutUpstream)
	}
}
