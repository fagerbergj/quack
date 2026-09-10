package tools

import "testing"

// Capped bounds a turn's rejection loop two ways - a hard round count, and
// an early stop once the judge repeats the identical reason (the QA rig hit
// 56 rejections over 347s with no cap: result-18a8d57.md).

func TestPlanCache_Capped_HitsRoundCapOnDistinctReasons(t *testing.T) {
	c := NewPlanCache()
	c.SetCaps(3, 100) // repeat cap high enough it never trips first

	for i, reason := range []string{"add a terminal node", "declare delivery", "wrong agent for the task"} {
		if capped, _ := c.Capped(); capped {
			t.Fatalf("round %d: capped before the round cap was reached", i+1)
		}
		c.RecordRejection(reason)
	}

	capped, reasons := c.Capped()
	if !capped {
		t.Fatal("3 rejections against a cap of 3 must trip Capped")
	}
	want := "add a terminal node; declare delivery; wrong agent for the task"
	if reasons != want {
		t.Errorf("reasons = %q, want %q", reasons, want)
	}
}

func TestPlanCache_Capped_StopsEarlyOnRepeatedReason(t *testing.T) {
	c := NewPlanCache()
	c.SetCaps(10, 3) // round cap high enough it never trips first

	const reason = "delivery not declared"
	c.RecordRejection(reason)
	if capped, _ := c.Capped(); capped {
		t.Fatal("one rejection must not trip the repeat cap of 3")
	}
	c.RecordRejection(reason)
	if capped, _ := c.Capped(); capped {
		t.Fatal("two identical rejections must not trip the repeat cap of 3")
	}
	c.RecordRejection(reason)

	capped, reasons := c.Capped()
	if !capped {
		t.Fatal("the same reason 3 times in a row must trip Capped - repeating the same feedback is not converging")
	}
	if reasons != reason {
		t.Errorf("reasons = %q, want the single deduped reason %q", reasons, reason)
	}
}

func TestPlanCache_Capped_RepeatStreakResetsOnNewReason(t *testing.T) {
	c := NewPlanCache()
	c.SetCaps(10, 3)

	c.RecordRejection("A")
	c.RecordRejection("A")
	c.RecordRejection("B") // breaks the streak
	c.RecordRejection("B")
	if capped, _ := c.Capped(); capped {
		t.Fatal("the repeat streak must reset on a new reason, not accumulate across different reasons")
	}
}

func TestPlanCache_SetCaps_IgnoresNonPositiveValues(t *testing.T) {
	c := NewPlanCache()
	c.SetCaps(0, -1) // must not zero out the caps - RecordRejection would trip on round 1

	c.RecordRejection("add a terminal node")
	if capped, _ := c.Capped(); capped {
		t.Fatal("SetCaps(0, -1) must leave the default caps in place, not disable them")
	}
}

// realRigRejectionStreak: 3 consecutive plan-judge rejections captured
// verbatim from the QA rig's non-convergent run
// (~/workspace/wt/qa/logs/server.log, entries 17-19) - the judge reworded
// the identical complaint (a code-implementer node instead of a reviewer)
// each round, so no two are byte-identical. Exact string equality never
// caught this as a streak on the real rig; that's the bug this fixes.
var realRigRejectionStreak = []string{
	`The user explicitly requested to "actually clone the repo, read the change, and carry out the review" but the proposed plan is a code-implementer node that appends a line to README.md. This does not perform a review; it performs an unrelated modification. The terminal node must be a reviewer or explainer that reads the existing changes and posts findings, not one that modifies files.`,
	`The request explicitly asks to "carry out the review" or execute tools to actually perform the work, but the proposed plan's terminal node is a code-implementer that modifies README.md. This does not deliver the requested review output; it delivers an unrelated code change. The plan lacks a terminal node that performs the actual code review and produces the review findings as the deliverable.`,
	`The user explicitly requested a review of changes, but the proposed plan is a code-implementer node that modifies the README.md file instead of reviewing any changes. The plan does not address the user's request to "read the change, and carry out the review".`,
}

func TestPlanCache_Capped_RigTextStreakFiresByRoundThree(t *testing.T) {
	if realRigRejectionStreak[0] == realRigRejectionStreak[1] || realRigRejectionStreak[1] == realRigRejectionStreak[2] {
		t.Fatal("fixture regressed: these must be distinct strings - exact equality is exactly what must NOT be relied on here")
	}

	c := NewPlanCache()
	c.SetCaps(100, 3) // round cap high enough it never trips first

	for i, reason := range realRigRejectionStreak {
		c.RecordRejection(reason)
		capped, _ := c.Capped()
		wantCapped := i == len(realRigRejectionStreak)-1
		if capped != wantCapped {
			t.Fatalf("round %d: Capped() = %v, want %v - real judge rewordings must count as the same repeated complaint", i+1, capped, wantCapped)
		}
	}
}
