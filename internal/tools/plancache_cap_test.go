package tools

import "testing"

// Capped bounds a turn's plan-judge rejection loop with a hard round count.

func TestPlanCache_Capped_TripsAtRoundCap(t *testing.T) {
	c := NewPlanCache()
	c.SetRoundCap(3)

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

func TestPlanCache_SetRoundCap_IgnoresNonPositiveValues(t *testing.T) {
	c := NewPlanCache()
	c.SetRoundCap(0) // must not disable the cap - RecordRejection would never trip it

	for i := 0; i < defaultPlanJudgeRoundCap-1; i++ {
		c.RecordRejection("add a terminal node")
	}
	if capped, _ := c.Capped(); capped {
		t.Fatal("one rejection short of the default cap must not be capped")
	}
	c.RecordRejection("add a terminal node")
	if capped, _ := c.Capped(); !capped {
		t.Fatal("SetRoundCap(0) must leave the default cap in place, not disable it")
	}
}
