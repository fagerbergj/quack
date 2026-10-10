// Workers and judge share one prefix-cached vLLM model, so stable bytes before the first changing byte
// save prefill. Pins a minimum shared prefix between revise rounds so a reorder that kills it fails.
package vetting

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// commonPrefixLen returns the length of the longest shared prefix of a and b.
func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// synthReviewTask is an 80KB+ synthetic PR-review task, big enough that the shared-prefix fraction
// reflects section order, not section size.
func synthReviewTask() string {
	var sb strings.Builder
	sb.WriteString("Review pull request #1304 in fagerbergj/quack.\n\n--- DIFF ---\n")
	for i := 0; i < 900; i++ {
		fmt.Fprintf(&sb, "+\tif err := store.lockFor(id).Lock(); err != nil { return fmt.Errorf(\"row %d: %%w\", err) }\n", i)
	}
	return sb.String()
}

// TestRevisePromptSharedPrefixAcrossRounds: revise round 2 shares at least 70% of round 1's bytes
// as a common prefix (measured 80%+ on this task).
func TestRevisePromptSharedPrefixAcrossRounds(t *testing.T) {
	task := synthReviewTask()
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: task}}}
	answer := strings.Repeat("The change looks correct because the per-id lock is acquired before the read.\n", 60)
	act := workerActivity{
		fetched: map[string]struct{}{},
		workspace: []wsOp{
			{tool: "read_file", detail: "read_file internal/recordstore/recordstore.go"},
			{tool: "grep", detail: "grep lockFor"},
		},
	}
	env1 := verdictEnvelope{Passed: false, Score: 0.33, Threshold: 0.66, Scoring: "lowest_criterion", Round: 1,
		JudgeFailures: []failureEntry{{Criterion: criterionSpec{Name: "grounding"}, Score: 0.33, Threshold: 0.66, Shortfall: "claim about lockFor unverified", Fix: "grep it"}}}
	env2 := verdictEnvelope{Passed: false, Score: 0.66, Threshold: 0.66, Scoring: "lowest_criterion", Round: 2,
		JudgeFailures: []failureEntry{{Criterion: criterionSpec{Name: "grounding"}, Score: 0.66, Threshold: 0.66, Shortfall: "still missing the deadlock case at line 412", Fix: "read the file"}}}

	r1 := contentPlainText(buildRevisionContent("", question, answer, env1, act, false, nil, nil))
	r2 := contentPlainText(buildRevisionContent("", question, answer, env2, act, false, nil, nil))

	shared := commonPrefixLen(r1, r2)
	frac := float64(shared) / float64(len(r2))
	t.Logf("revise prompt len r1=%d r2=%d; shared prefix=%d bytes (%.1f%%)", len(r1), len(r2), shared, 100*frac)
	const minFraction = 0.70
	if frac < minFraction {
		t.Fatalf("revise prompt round1->round2 shared prefix = %.1f%% of round 2, want >= %.0f%% - "+
			"a section that changes every round has moved ahead of the original question, killing the prefix cache",
			100*frac, 100*minFraction)
	}

	// The revise prompt also shares a prefix with the draft round's raw task text.
	draftShared := commonPrefixLen(task, r1)
	if draftShared == 0 {
		t.Fatalf("revise round 1 shares 0 bytes with the draft round's question - the original question is no longer first")
	}
	t.Logf("draft->revise1 shared prefix = %d bytes", draftShared)
}

// TestBuildRevisionContentSectionsPresent: stable-first ordering drops no section the worker needs.
func TestBuildRevisionContentSectionsPresent(t *testing.T) {
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "the original question text"}}}
	env := verdictEnvelope{Passed: false, Score: 0.3, Threshold: 0.7, Round: 1,
		JudgeFailures: []failureEntry{{Criterion: criterionSpec{Name: "grounding"}, Shortfall: "the shortfall text", Fix: "the fix text"}}}

	got := contentPlainText(buildRevisionContent("the principles", question, "the previous answer text", env, workerActivity{}, false, nil, nil))

	sections := []string{"the original question text", "An independent reviewer evaluated", "the principles", "Verdict:", "the shortfall text", "the previous answer text"}
	last := -1
	for _, s := range sections {
		idx := strings.Index(got, s)
		if idx < 0 {
			t.Fatalf("revise prompt missing section %q:\n%s", s, got)
		}
		if idx < last {
			t.Fatalf("section %q is out of order (want question, instructions, principles, verdict, previous answer):\n%s", s, got)
		}
		last = idx
	}
}

// A revise reply becomes the user-facing answer, so the rule must ask for that answer
// again rather than a change log (QA: a lineup answer came back as "Revised the lineup artifact ...").
func TestBuildRevisionContentAsksForTheAnswerNotAChangeNote(t *testing.T) {
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Set my lineup."}}}
	env := verdictEnvelope{Threshold: 0.6, Round: 1}
	for _, citationOnly := range []bool{false, true} {
		got := contentPlainText(buildRevisionContent("", question, "the lineup summary", env, workerActivity{}, citationOnly, nil, nil))
		if !strings.Contains(got, "never a note about this revision") || strings.Contains(got, "note of what changed") {
			t.Errorf("citationOnly=%v: revise prompt must ask for the user-facing answer, not a change note:\n%s", citationOnly, got)
		}
	}
}
