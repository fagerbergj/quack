package dag

import "testing"

// Only a worker round is aborted for a steer: a judge call registers just the stop abort, so a steer that
// lands while judging waits for the gate boundary instead of killing the judge.
func TestEnqueue_AbortsOnlyAWorkerRound(t *testing.T) {
	c := &nodeControl{}
	judgeAborted := false
	c.SetRoundAbort(func() { judgeAborted = true })
	c.enqueue("during judging")
	if judgeAborted || c.SteerInterrupted() {
		t.Fatalf("judge aborted = %v; a steer must not abort a judge call", judgeAborted)
	}
	c.ClearRoundAbort()

	workerAborted := false
	c.SetWorkerRound(func() { workerAborted = true })
	c.enqueue("during the worker round")
	if !workerAborted || !c.SteerInterrupted() {
		t.Fatalf("worker aborted = %v; a steer must interrupt the running worker round", workerAborted)
	}
	if c.SteerInterrupted() {
		t.Error("SteerInterrupted must clear once read")
	}
}

// A flag the gate never read (the round finished first) must not interrupt the next round.
func TestSetWorkerRound_ClearsAStaleSteerFlag(t *testing.T) {
	c := &nodeControl{}
	c.SetWorkerRound(func() {})
	c.enqueue("late")
	c.ClearWorkerRound()
	c.SetWorkerRound(func() {})
	if c.SteerInterrupted() {
		t.Error("a new worker round inherited the previous round's steer flag")
	}
}
