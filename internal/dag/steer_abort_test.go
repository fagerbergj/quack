package dag

import "testing"

// Only a worker round is aborted for a steer: a judge call registers just the stop abort, so a steer that
// lands while judging waits for the gate boundary instead of killing the judge.
func TestEnqueue_AbortsOnlyAWorkerRound(t *testing.T) {
	c := &nodeControl{}
	judgeAborted := false
	c.SetRoundAbort(func() { judgeAborted = true })
	c.enqueue("during judging")
	if judgeAborted || c.RoundInterrupted() {
		t.Fatalf("judge aborted = %v; a steer must not abort a judge call", judgeAborted)
	}
	c.ClearRoundAbort()

	workerAborted := false
	c.SetWorkerRound(func() { workerAborted = true })
	c.enqueue("during the worker round")
	if !workerAborted || !c.RoundInterrupted() {
		t.Fatalf("worker aborted = %v; a steer must interrupt the running worker round", workerAborted)
	}
	if c.RoundInterrupted() {
		t.Error("RoundInterrupted must clear once read")
	}
}

// A flag the gate never read (the round finished first) must not interrupt the next round.
func TestSetWorkerRound_ClearsAStaleSteerFlag(t *testing.T) {
	c := &nodeControl{}
	c.SetWorkerRound(func() {})
	c.enqueue("late")
	c.ClearWorkerRound()
	c.SetWorkerRound(func() {})
	if c.RoundInterrupted() {
		t.Error("a new worker round inherited the previous round's steer flag")
	}
}

// A user pause aborts the worker round but never a judge call; a shutdown pause stays cooperative and must not
// relabel a user pause, or boot would resume a node the user paused.
func TestPauseNode_AbortsOnlyAUserPausedWorkerRound(t *testing.T) {
	ex := &Executor{controls: newRunControls()}
	c, _, _ := ex.controls.register("chat", "n1")
	aborted := false
	c.SetRoundAbort(func() { aborted = true })
	ex.PauseNode("chat", "n1", PauseUser)
	if aborted || c.RoundInterrupted() {
		t.Fatal("a pause aborted a judge call")
	}
	c.ClearRoundAbort()
	c.resume()

	c.SetWorkerRound(func() { aborted = true })
	ex.PauseNode("chat", "n1", PauseShutdown)
	if aborted {
		t.Fatal("a shutdown pause aborted the worker round")
	}
	c.resume()
	ex.PauseNode("chat", "n1", PauseUser)
	if !aborted || !c.RoundInterrupted() {
		t.Fatal("a user pause must interrupt the running worker round")
	}
	ex.PauseNode("chat", "n1", PauseShutdown)
	if r := c.PauseReason(); r != PauseUser {
		t.Errorf("pause reason = %q after a shutdown drain, want it kept as user", r)
	}
}
