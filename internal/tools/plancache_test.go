package tools

import "testing"

func TestPlanCachePending(t *testing.T) {
	c := NewPlanCache()
	if c.Pending() != nil {
		t.Fatal("fresh cache should have no pending input")
	}
	p := &PendingInput{NodeID: "n1", CallID: "c1"}
	c.SetPending(p)
	if got := c.Pending(); got == nil || got.NodeID != "n1" {
		t.Errorf("Pending() = %+v, want the set snapshot", got)
	}
	c.ClearPending()
	if c.Pending() != nil {
		t.Error("ClearPending should drop the pending input")
	}
}
