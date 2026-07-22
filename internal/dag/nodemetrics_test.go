package dag

import (
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/stream"
)

// A finished node must report what it cost: how long it took, and what the trust gate
// said. All three numbers were broken at once, and together they made a node's
// performance invisible - which is why "the explorers run for hours" went unnoticed for
// so long.
//
// Live (2026-07-13), a node whose judge PASSED at 1.0 persisted:
//
//		explorer-goose | done | duration_ms=0 | judge_rounds=0 | judge_final_score=0 | started_at=NULL
//
//	  - duration_ms was never assigned in nodeDoneData at all - structurally always 0.
//	  - the judge fields were read back with a fresh sessions.Get, but the gated node
//	    writes them as a STATE DELTA that has not been appended when node_done is built,
//	    so the read saw nothing. (Confirmed: no session in the database contained a
//	    gate_score key.)
//	  - started_at was nulled by the node_done upsert (see store.UpsertDagNode).
func TestNodeDoneReportsDurationAndGateResult(t *testing.T) {
	const node = "explorer-goose"

	ds := newDagStream(
		map[string]string{node: "code-explorer"},
		func(stream.SSEEvent, error) bool { return true },
		map[string]string{node: "goose registers tools via ExtensionManager…"},
		func(id string) gateScore { return gateScore{score: 1.0, passed: true, rounds: 1} },
		func(string) bool { return false },
		func(string) bool { return false },
		func(string, int) string { return "" },
	)

	// The node ran: mark it started (this is what emitting node_start does).
	ds.started[node] = true
	ds.startedAt[node] = nowMinus(t, 90) // 90s ago

	d := ds.nodeDoneData(node)

	if d.DurationMs <= 0 {
		t.Errorf("node_done reports duration_ms=%d - a completed node must say how long it took", d.DurationMs)
	}
	if d.DurationMs < 89_000 {
		t.Errorf("duration_ms=%d, want ~90s", d.DurationMs)
	}
	if d.JudgeFinalScore != 1.0 || !d.JudgePassed || d.JudgeRounds != 1 {
		t.Errorf("node_done lost the judge result: score=%v passed=%v rounds=%d - the node claims its trust gate never ran",
			d.JudgeFinalScore, d.JudgePassed, d.JudgeRounds)
	}
}

// The in-memory gate result is what node_done actually reads: the session-state write
// isn't visible to a fresh Get at that moment. Recording it must round-trip.
func TestRecordedGateResultIsReadableImmediately(t *testing.T) {
	e := &Executor{}
	e.recordGateResult("chat-1", "n1", 0.85, true, 2)

	got := e.gateScore(t.Context(), "quack", "local", "chat-1", "n1")
	if got.score != 0.85 || !got.passed || got.rounds != 2 {
		t.Fatalf("gateScore = %+v, want {0.85 true 2} - node_done cannot see the gate's own result", got)
	}

	// A different chat with the same node id must not collide.
	if other := e.gateScore(t.Context(), "quack", "local", "chat-2", "n1"); other.score != 0 {
		t.Fatalf("gate result leaked across chats: %+v", other)
	}
}

// nowMinus returns a time n seconds in the past.
func nowMinus(t *testing.T, secs int) time.Time {
	t.Helper()
	return time.Now().Add(-time.Duration(secs) * time.Second)
}
