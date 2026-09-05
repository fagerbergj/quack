package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
)

// writeToolReplayFixture writes a one-entry ledger JSONL recording a single
// execute_tool call for current_date in node "node-a".
func writeToolReplayFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"seq":1,"chat_id":"c","kind":"tool.call","at":"2026-01-01T00:00:00Z",` +
		`"node_id":"node-a","agent":"worker","round":"worker-r0","payload":{` +
		`"name":"current_date","result":"{\"result\":\"Today's date is recorded-day.\"}"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuild_StrictReplay_NeverConstructsRealTool: Deps.Replayer in strict
// mode returns a stub with NO live delegate at all - Build never reaches the
// registry constructor, so a strict replay run has zero real-backend
// dependency by construction (.quack/replay-log.md).
func TestBuild_StrictReplay_NeverConstructsRealTool(t *testing.T) {
	sess, err := replay.Load(writeToolReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tools, err := Build([]string{"current_date"}, Deps{Replayer: sess})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	stub, ok := tools[0].(*replayToolStub)
	if !ok {
		t.Fatalf("tools[0] = %T, want *replayToolStub", tools[0])
	}
	if stub.live != nil {
		t.Errorf("live = %v, want nil in strict mode", stub.live)
	}
}

// TestBuild_ForkReplay_FallsThroughToARealTool: fork mode wires a REAL
// current_date tool as the stub's live delegate (Build's recursive,
// Replayer-cleared call), and a call the recording can't satisfy (a second
// call - only one was recorded) is answered by it - proven directly on the
// replayToolStub (agent.Context plumbing for a real functiontool call is
// replaytest's job, not this unit test's - see TestBuild_StrictReplay_
// NeverConstructsRealTool for the strict-mode half of this same wiring).
func TestBuild_ForkReplay_FallsThroughToARealTool(t *testing.T) {
	sess, err := replay.Load(writeToolReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("")
	built, err := Build([]string{"current_date"}, Deps{Replayer: sess})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	stub, ok := built[0].(*replayToolStub)
	if !ok {
		t.Fatalf("built[0] = %T, want *replayToolStub", built[0])
	}
	if stub.live == nil {
		t.Fatal("live = nil, want a real current_date tool wired as the fork-mode fallback")
	}
	if stub.live.Name() != "current_date" {
		t.Errorf("live.Name() = %q, want current_date", stub.live.Name())
	}

	coords := ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"}
	// First call replays the recorded result.
	res, err := stub.session.NextToolResult(coords, "current_date", nil)
	if err != nil {
		t.Fatalf("first NextToolResult: %v", err)
	}
	if s, _ := res["result"].(string); s != "Today's date is recorded-day." {
		t.Errorf("first result = %+v, want the recorded string", res)
	}
	// Second call has nothing left recorded - fork mode must hand this off
	// to stub.live rather than fail (asserted at the engine level already
	// in internal/replay/fork_test.go; here we only need stub.live wired).
	_, err = stub.session.NextToolResult(coords, "current_date", nil)
	var fs *replay.ForkSignal
	if !errors.As(err, &fs) {
		t.Fatalf("second NextToolResult: err = %v, want a *replay.ForkSignal", err)
	}
}

// TestBuild_ForkReplay_NoLiveDelegateFailsLoudly: an unbuildable live tool
// name (unknown to the registry) fails Build outright rather than silently
// dropping the fallback.
func TestBuild_ForkReplay_NoLiveDelegateFailsLoudly(t *testing.T) {
	sess, err := replay.Load(writeToolReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("")
	_, err = Build([]string{"not_a_real_tool"}, Deps{Replayer: sess})
	if err == nil {
		t.Fatal("want an error building fork mode's live delegate for an unknown tool")
	}
	var unused *replay.ForkSignal
	if errors.As(err, &unused) {
		t.Errorf("Build's own error should not itself be a ForkSignal")
	}
}
