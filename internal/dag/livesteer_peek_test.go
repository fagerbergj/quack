package dag

import "testing"

// #1029 review: live delivery must PEEK. TakeQueued marks Delivered, records a
// -sN generation in drained and persists; if the live callback consumed the
// queue it would (1) burn a generation with no matching -sN run, so node_steered
// resolves to the wrong text, (2) drop the message before it reaches the durable
// prompt, and (3) lose it entirely if that model call then fails.
func TestPeekQueued_DoesNotConsumeOrRecordAGeneration(t *testing.T) {
	c := &nodeControl{}
	c.enqueue("FIRST steer")

	if got := c.PeekQueued(); got != "FIRST steer" {
		t.Fatalf("PeekQueued = %q, want the pending message", got)
	}
	// Peeking twice still sees it: nothing was consumed.
	if got := c.PeekQueued(); got != "FIRST steer" {
		t.Fatalf("second PeekQueued = %q, want the message still pending", got)
	}
	if len(c.drained) != 0 {
		t.Fatalf("peek recorded %d steer generation(s); it must record none", len(c.drained))
	}

	// The gate boundary still owns durable delivery.
	if got := c.TakeQueued(); got != "FIRST steer" {
		t.Fatalf("TakeQueued = %q, want the peeked message still deliverable", got)
	}
	if len(c.drained) != 1 {
		t.Fatalf("drained = %d generations after one gate drain, want 1", len(c.drained))
	}
	if got := c.PeekQueued(); got != "" {
		t.Errorf("PeekQueued after the gate drained it = %q, want empty", got)
	}
}

// The -sN generation the UI resolves must stay aligned with the gate drains
// that actually produced a run, not be shifted by live deliveries.
func TestPeekQueued_KeepsSteerGenerationsAligned(t *testing.T) {
	c := &nodeControl{}
	c.enqueue("FIRST steer")
	c.PeekQueued() // live delivery, mid-round
	c.TakeQueued() // gate boundary -> generation 1 (run "-s1")
	c.enqueue("SECOND steer")
	c.PeekQueued()
	c.TakeQueued() // generation 2 (run "-s2")

	if len(c.drained) != 2 {
		t.Fatalf("drained = %d, want exactly one generation per gate drain", len(c.drained))
	}
	if got := c.drained[0][0]; got != "FIRST steer" {
		t.Errorf("generation 1 = %q, want the first steer", got)
	}
	if got := c.drained[1][0]; got != "SECOND steer" {
		t.Errorf("generation 2 = %q, want the second steer", got)
	}
}

// The tests above pin PeekQueued itself; this pins the WIRING. Assembly must
// hand nodes a NON-consuming drain - swapping it back to TakeQueued leaves the
// others green while silently reintroducing all three review findings.
func TestBuildGateNodes_LiveDrainDoesNotConsumeTheQueue(t *testing.T) {
	const chatID, nodeID = "chat-peek", "n1"
	rc := newRunControls()
	c, _, _ := rc.register(chatID, nodeID)
	defer rc.unregister(chatID, nodeID)
	c.enqueue("STOP RESEARCHING")

	drain := liveSteerDrain(rc, chatID, nodeID)
	if got := drain(); got != "STOP RESEARCHING" {
		t.Fatalf("live drain = %q, want the pending steer", got)
	}
	if got := drain(); got != "STOP RESEARCHING" {
		t.Fatalf("live drain consumed the queue (got %q on the second call) - the gate boundary can no longer deliver it durably", got)
	}
	if len(c.drained) != 0 {
		t.Errorf("live drain recorded %d steer generation(s); node_steered would resolve to the wrong text", len(c.drained))
	}
}
