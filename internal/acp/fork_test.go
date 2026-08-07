package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/workspace"
)

// emptyReplaySession loads a Session with nothing recorded for it - every
// NextInvokeAgent call on it is, by construction, a "never recorded"
// extra-call miss (replay.ClassExtra, position 0).
func emptyReplaySession(t *testing.T) *replay.Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sess, err := replay.Load(path)
	if err != nil {
		t.Fatalf("replay.Load: %v", err)
	}
	return sess
}

// TestFork_MissFallsThroughToARealSubprocess: a coords/agent the recording
// never made an invoke_agent call for - #604's own TestPlayback_
// MissingExchangeIsMissError case, but in FORK mode - falls through to a
// REAL subprocess round instead of failing (#605). The fake command is
// wired for real (unlike playback_test.go's "/nonexistent/..." binary,
// which proves playback never spawns) so a live round can actually succeed.
func TestFork_MissFallsThroughToARealSubprocess(t *testing.T) {
	sess := emptyReplaySession(t)
	sess.EnableFork("") // fork purely on divergence

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=happy"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		Replay:  sess,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	coords := ledger.Coords{ChatID: "c1", Node: "n1", Agent: "code-implementer", Round: "worker-r0"}
	ctx := ledger.WithCoords(context.Background(), coords)

	var specs []eventSpec
	if err := a.round(ctx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	}); err != nil {
		t.Fatalf("round: %v, want it to fork to a live subprocess and succeed", err)
	}
	if len(specs) == 0 {
		t.Fatal("no events emitted from the forked-live round")
	}
	final := specs[len(specs)-1]
	if final.partial || final.parts[0].Text != "did the thing" {
		t.Fatalf("final answer = %q partial=%v, want the fake agent's real (\"happy\" mode) answer", final.parts[0].Text, final.partial)
	}

	rep := sess.Report()
	if len(rep.Forked) != 1 || rep.Forked[0].Reason != "miss" {
		t.Errorf("Report.Forked = %+v, want exactly one miss-triggered fork", rep.Forked)
	}
}

// TestFork_StrictModeStillNeverSpawns: EnableFork not called (strict, #604's
// existing guarantee) - the SAME never-recorded coords still fails outright
// against the unreachable command, proving #605 didn't loosen replay-strict.
func TestFork_StrictModeStillNeverSpawns(t *testing.T) {
	sess := emptyReplaySession(t) // mode left at ModeStrict

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command: []string{"/nonexistent/opencode-must-not-spawn"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		Replay:  sess,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	coords := ledger.Coords{ChatID: "c1", Node: "n1", Agent: "code-implementer", Round: "worker-r0"}
	ctx := ledger.WithCoords(context.Background(), coords)
	err = a.round(ctx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("want an error - strict mode must never fall through to a live spawn")
	}
}
