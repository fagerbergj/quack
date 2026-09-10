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
