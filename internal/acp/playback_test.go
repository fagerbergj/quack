package acp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestPlayback_ReplaysRecordedRoundWithNoSubprocess records one real round
// through the fake subprocess via the real emission seam, then replays it
// through Options.Replay - no subprocess, no Command that could even be
// spawned - and asserts the gate-visible activity (thought, durable
// run_command tool pair, final answer text) matches the recording (#604).
func TestPlayback_ReplaysRecordedRoundWithNoSubprocess(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	restore := otelobs.SetLoggerProviderForTesting(lp)

	coords := ledger.Coords{ChatID: "c1", Node: "n1", Agent: "code-implementer", Round: "worker-r0"}
	recordCtx := ledger.WithCoords(context.Background(), coords)

	live := testAgent(t, "happy")
	var recorded []eventSpec
	if err := live.round(recordCtx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(s eventSpec) bool {
		recorded = append(recorded, s)
		return true
	}); err != nil {
		t.Fatalf("recording round: %v", err)
	}
	if err := lp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	restore() // stop recording before replay - the two must not interfere

	sess, err := replay.Load(filepath.Join(root, coords.ChatID+".jsonl"))
	if err != nil {
		t.Fatalf("replay.Load: %v", err)
	}

	// Command names a binary that does not exist - if playback ever fell
	// through to a real spawn, this proves it by failing loudly instead of
	// silently succeeding against a real subprocess.
	jail := live.opts.Jail
	replayed, err := New("code-implementer", "external coder", Options{
		Command: []string{"/nonexistent/opencode-must-not-spawn"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		Replay:  sess,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var specs []eventSpec
	replayCtx := ledger.WithCoords(context.Background(), coords)
	if err := replayed.round(replayCtx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	}); err != nil {
		t.Fatalf("replayed round: %v", err)
	}

	if len(specs) == 0 {
		t.Fatal("no events emitted from playback")
	}
	final := specs[len(specs)-1]
	wantFinal := recorded[len(recorded)-1]
	if final.parts[0].Text != wantFinal.parts[0].Text {
		t.Errorf("final answer = %q, want %q (the recorded round's answer)", final.parts[0].Text, wantFinal.parts[0].Text)
	}
	var sawThought, sawPair bool
	for _, s := range specs {
		for _, p := range s.parts {
			if p.Thought && p.Text == "planning" {
				sawThought = true
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "run_command" && !s.partial {
				sawPair = true
			}
		}
	}
	if !sawThought || !sawPair {
		t.Fatalf("replayed stream incomplete: thought=%v durable run_command pair=%v", sawThought, sawPair)
	}
}

// TestPlayback_MissingExchangeIsMissError: a coords/agent the recording
// never made an invoke_agent call for replays as a structured MissError
// (stream + position), never a bare "not found" or a silent empty round -
// the ACP twin of replay's model/tool "extra call" acceptance case.
func TestPlayback_MissingExchangeIsMissError(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	restore := otelobs.SetLoggerProviderForTesting(lp)

	coords := ledger.Coords{ChatID: "c2", Node: "n1", Agent: "code-implementer", Round: "worker-r0"}
	recordCtx := ledger.WithCoords(context.Background(), coords)
	live := testAgent(t, "happy")
	if err := live.round(recordCtx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("recording round: %v", err)
	}
	if err := lp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	restore()

	sess, err := replay.Load(filepath.Join(root, coords.ChatID+".jsonl"))
	if err != nil {
		t.Fatalf("replay.Load: %v", err)
	}

	replayed, err := New("code-implementer", "external coder", Options{
		Command: []string{"/nonexistent/opencode-must-not-spawn"},
		Home:    t.TempDir(),
		Jail:    live.opts.Jail,
		UserID:  "u1",
		Replay:  sess,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First round against this stream consumes the ONE recorded invoke_agent
	// entry; a SECOND replays against the same (now-exhausted) stream and
	// must miss.
	replayCtx := ledger.WithCoords(context.Background(), coords)
	if err := replayed.round(replayCtx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("first replayed round: %v", err)
	}
	err = replayed.round(replayCtx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("want an error from replaying past the recorded stream")
	}
	var missErr *replay.MissError
	if !errors.As(err, &missErr) {
		t.Fatalf("err = %v, want it to wrap a *replay.MissError", err)
	}
	if missErr.Class != replay.ClassExtra {
		t.Errorf("Class = %q, want extra", missErr.Class)
	}
	if missErr.Stream.Node != coords.Node || missErr.Stream.Agent != coords.Agent || missErr.Stream.Round != coords.Round {
		t.Errorf("Stream = %+v, want %+v", missErr.Stream, coords)
	}
}
