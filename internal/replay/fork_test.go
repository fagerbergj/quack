package replay

import (
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestEnableFork_MissForksInsteadOfFailing: strict-mode's exhausted-stream
// miss becomes a *ForkSignal in fork mode, and is reported under Forked, not
// Failures - the whole point of fork-replay (.quack/replay-log.md "goes live
// from the first divergent step").
func TestEnableFork_MissForksInsteadOfFailing(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("") // no explicit boundary - fork purely on divergence
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}

	if _, err := sess.NextChat(coords, "worker-model", nil); err != nil {
		t.Fatalf("first NextChat (recorded): %v", err)
	}
	// Second call has nothing left recorded - strict mode would fail here.
	_, err = sess.NextChat(coords, "worker-model", nil)
	var fs *ForkSignal
	if !errors.As(err, &fs) {
		t.Fatalf("second NextChat: err = %v (%T), want *ForkSignal", err, err)
	}
	if fs.Reason != "miss" || fs.Cause == nil || fs.Cause.Class != ClassExtra {
		t.Errorf("ForkSignal = %+v, want Reason=miss with an extra-call Cause", fs)
	}

	rep := sess.Report()
	if len(rep.Failures) != 0 {
		t.Errorf("Failures = %+v, want none (a fork is not a failure)", rep.Failures)
	}
	if len(rep.Forked) != 1 || rep.Forked[0].Stream.Node != "node-a" {
		t.Errorf("Forked = %+v, want exactly one entry for node-a", rep.Forked)
	}
}

// TestEnableFork_Sticky: once a stream has forked, EVERY later call on it
// (even one that WOULD have matched a still-unconsumed recorded entry)
// keeps going live - a round shouldn't flip-flop between recorded and live
// mid-stream.
func TestEnableFork_Sticky(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("")
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}

	// A MISMATCHED call forks the stream even though position 0 has a
	// recorded entry (just for a different model name).
	_, err = sess.NextChat(coords, "some-other-model", nil)
	var fs *ForkSignal
	if !errors.As(err, &fs) || fs.Reason != "miss" {
		t.Fatalf("first NextChat: err = %v, want a miss ForkSignal", err)
	}
	// Second call would match the SECOND recorded entry (same model name) -
	// but the stream is already forked, so it must go live again, not replay.
	_, err = sess.NextChat(coords, "worker-model", nil)
	if !errors.As(err, &fs) || fs.Reason != "sticky" {
		t.Fatalf("second NextChat: err = %v, want a sticky ForkSignal", err)
	}
}

// TestEnableFork_ExplicitForkFrom: --fork-from's node boundary forces a
// stream live from its VERY FIRST call, even when the recording has a
// perfectly matching entry waiting - verifying a prompt/plan fix needs a
// real model call, not the old recorded response, regardless of whether the
// call sequence itself would have diverged.
func TestEnableFork_ExplicitForkFrom(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-a", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("node-a")
	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}

	_, err = sess.NextChat(coords, "worker-model", nil) // identity matches the recording exactly
	var fs *ForkSignal
	if !errors.As(err, &fs) || fs.Reason != "fork-from" {
		t.Fatalf("NextChat = %v, want an explicit fork-from ForkSignal despite a matching recorded entry", err)
	}

	rep := sess.Report()
	if len(rep.Forked) != 1 {
		t.Errorf("Forked = %+v, want exactly one entry", rep.Forked)
	}
	// The stream was never actually consumed - the recorded entry is left
	// untouched, informational only via Streams' consumed/total tally.
	for _, sr := range rep.Streams {
		if sr.Stream.Node == "node-a" && sr.Consumed != 0 {
			t.Errorf("stream %s consumed = %d, want 0 (forked before touching the recording)", sr.Stream, sr.Consumed)
		}
	}
}

// TestEnableFork_ForkFromScopedToItsOwnNode: --fork-from names ONE node - a
// different node's stream, with no divergence of its own, keeps replaying
// normally (fork-replay only forces the NAMED node live; anything else
// forks only on its own genuine divergence - see forkOrFail).
func TestEnableFork_ForkFromScopedToItsOwnNode(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-b", "worker", "worker-r0", "worker-model", nil),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("node-a") // a DIFFERENT node than the one queried below
	coords := ledger.Coords{Node: "node-b", Agent: "worker", Round: "worker-r0"}

	if _, err := sess.NextChat(coords, "worker-model", nil); err != nil {
		t.Fatalf("NextChat on an unrelated node: %v, want a normal replay", err)
	}
	if rep := sess.Report(); len(rep.Forked) != 0 {
		t.Errorf("Forked = %+v, want none - node-b never diverged and isn't the fork boundary", rep.Forked)
	}
}

// TestEnableFork_ToolAndAgentStreamsForkToo: the SAME fork machinery applies
// to NextToolResult and NextInvokeAgent, not just NextChat.
func TestEnableFork_ToolAndAgentStreamsForkToo(t *testing.T) {
	sess, err := Load(writeJSONL(t, nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("node-a")

	toolCoords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	_, err = sess.NextToolResult(toolCoords, "web_search", nil)
	var fs *ForkSignal
	if !errors.As(err, &fs) || fs.Reason != "fork-from" {
		t.Fatalf("NextToolResult = %v, want a fork-from ForkSignal", err)
	}

	agentCoords := ledger.Coords{Node: "node-a", Agent: "code-implementer", Round: "worker-r0"}
	_, _, err = sess.NextInvokeAgent(agentCoords, "code-implementer")
	if !errors.As(err, &fs) || fs.Reason != "fork-from" {
		t.Fatalf("NextInvokeAgent = %v, want a fork-from ForkSignal", err)
	}
}

// TestSession_ModeDefaultsStrict pins EnableFork's precondition: a Session
// nobody calls it on stays ModeStrict (the zero value's meaning), so every
// existing #603/#604 caller (NewModel's kind:"replay", replaytest, ACP
// playback) is unaffected by #605's addition.
func TestSession_ModeDefaultsStrict(t *testing.T) {
	sess, err := Load(writeJSONL(t, nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.Mode() != ModeStrict {
		t.Errorf("Mode() = %q, want %q", sess.Mode(), ModeStrict)
	}
}
