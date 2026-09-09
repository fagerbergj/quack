// judge_prefix_cache_test.go: regression test for finding 4 of the agent-efficiency
// audit (stable-first section ordering in buildJudgePrompt - the answer being judged
// trails last instead of leading). See revise_prefix_cache_test.go's doc comment for
// why this matters: a byte stable across rounds and ahead of the first changing byte
// is a prefill saved under prod's shared vLLM prefix cache.
package vetting

import (
	"strings"
	"testing"
)

// TestJudgePromptSharedPrefixAcrossRounds pins finding 4: on the same node,
// round 2 (a different answer, same everything else) must share at least 85%
// of round 1's bytes as a common prefix - the fraction the reorder measured
// at 91.7% on the audit's synthetic task, comfortably above the pre-fix 7.8%.
func TestJudgePromptSharedPrefixAcrossRounds(t *testing.T) {
	task := "Review pull request and post inline findings."
	question := questionContent("Review PR in fagerbergj/quack")
	rubric := strings.Repeat("criterion: grounding - every claim about this repo must be verified by reading it.\n", 40)
	constitution := strings.Repeat("Be precise. Do not fabricate.\n", 10)
	diff := "Changed files under review (diff of the clone):\n" + synthReviewTask()
	act := workerActivity{workspace: []wsOp{{tool: "read_file", detail: "read_file internal/recordstore/recordstore.go"}, {tool: "grep", detail: "grep lockFor"}}}
	ans1 := strings.Repeat("Round 1 answer: the per-id lock is correct.\n", 40)
	ans2 := strings.Repeat("Round 2 answer: the per-id lock is correct and the map is guarded.\n", 40)

	p1 := buildJudgePrompt(constitution, rubric, task, question, ans1, diff, act, "")
	p2 := buildJudgePrompt(constitution, rubric, task, question, ans2, diff, act, "")

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
