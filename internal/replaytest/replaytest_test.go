package replaytest

// The three spec test cases (.quack/replay-log.md "Test cases" 1-3, issue
// #603's acceptance criteria) against ONE shared fixture bundle (a real
// recorded 2-node gated-refine run - see fixture_test.go).

import (
	"errors"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/replay"
)

// fixtureInstruction is the fixture worker's own instruction verbatim (see
// fixture_test.go's runFixtureNode) - matching it exactly is what makes
// case 1 "zero drift"; case 2 deliberately diverges from it.
const fixtureInstruction = "Answer the question. Use web_search first."

func nodeAOptions() Options {
	return Options{
		NodeID:      fixtureNodeA,
		WorkerModel: fixtureWorkerModel,
		JudgeModel:  fixtureJudgeModel,
		ToolNames:   []string{fixtureToolName},
		Cfg:         fixtureCfg(fixtureNodeA),
		Instruction: fixtureInstruction,
	}
}

// TestCase1_RecordedRunReplaysGreen: the harness-regression case. Replaying
// the fixture unmodified must reproduce the recorded outcome (a revised,
// judge-passed answer) with a completely clean divergence report - every
// stream fully consumed, no drift, no structural failures.
func TestCase1_RecordedRunReplaysGreen(t *testing.T) {
	bundle := buildFixtureBundle(t)

	out := Run(t, bundle, nodeAOptions())

	if out.Err != nil {
		t.Fatalf("replay run failed: %v", out.Err)
	}
	if !out.Result.Passed {
		t.Errorf("Result.Passed = false, want true (recorded run passed the judge)")
	}
	if !strings.Contains(out.Answer, "revised") {
		t.Errorf("Answer = %q, want the revised answer (matches the recording)", out.Answer)
	}
	// The fixture bundle records TWO nodes; this harness drives only
	// node-a (the documented single-node ceiling - see replaytest.go), so
	// node-b's streams are legitimately untouched here, not a divergence.
	// "Zero divergence" for THIS run means: no drift, no structural
	// failures, and every node-a stream fully consumed.
	if len(out.Report.Drift) != 0 || len(out.Report.Failures) != 0 {
		t.Errorf("Report = %+v, want no drift/failures", out.Report)
	}
	var touchedNodeA bool
	for _, s := range out.Report.Streams {
		if s.Stream.Node != fixtureNodeA {
			continue
		}
		touchedNodeA = true
		if s.Consumed != s.Total {
			t.Errorf("stream %s %s: consumed %d/%d, want fully consumed", s.Stream, s.Op, s.Consumed, s.Total)
		}
	}
	if !touchedNodeA {
		t.Errorf("Report.Streams has no node-a entries - the replay never touched the recorded stream(s)")
	}
}

// TestCase2_PromptEditTolerated: changing the live worker's system
// instruction must still replay green - the recorded model/judge responses
// don't depend on it - but the report must list the resulting prompt-hash
// drift, and ONLY that.
func TestCase2_PromptEditTolerated(t *testing.T) {
	bundle := buildFixtureBundle(t)

	opts := nodeAOptions()
	opts.Instruction = "Answer the question CAREFULLY. Use web_search first." // edited from the recording
	out := Run(t, bundle, opts)
	if out.Err != nil {
		t.Fatalf("replay run failed: %v", out.Err)
	}
	if !out.Result.Passed {
		t.Errorf("Result.Passed = false, want true")
	}
	if len(out.Report.Failures) != 0 {
		t.Errorf("Failures = %+v, want none", out.Report.Failures)
	}
	// The worker instruction above deliberately differs from the fixture's
	// own recorded one (fixtureInstruction) - identity (model name) still
	// matches, so the run replays green; ONLY the prompt-version hash
	// disagrees, which the report should list as drift, nothing else.
	if len(out.Report.Drift) == 0 {
		t.Errorf("Drift is empty, want at least one prompt-version drift record (the worker instruction differs from the recording)")
	}
	for _, d := range out.Report.Drift {
		if d.Stream.Agent != "worker" {
			t.Errorf("drift on stream %s, want only the worker stream to drift", d.Stream)
		}
	}
}

// TestCase3_ExtraCallFailsLoudly: the gate demanding a higher score than
// what was recorded forces a live round the recording never made. Replay
// must fail at the exact stream + position, with a near-miss diff, never a
// bare "not found" - captured in the divergence report even though the
// gate's own graceful-degradation design (a revision-worker failure keeps
// the PRIOR answer rather than aborting the node) means RunGatedRefine
// itself returns no error here; the report is what "fails loudly" refers to.
func TestCase3_ExtraCallFailsLoudly(t *testing.T) {
	bundle := buildFixtureBundle(t)

	opts := nodeAOptions()
	opts.Cfg.Threshold = 0.99 // higher than the recorded passing score (0.9)
	out := Run(t, bundle, opts)

	if len(out.Report.Failures) != 1 {
		t.Fatalf("Report.Failures = %d, want exactly 1 (got %+v; err=%v)", len(out.Report.Failures), out.Report.Failures, out.Err)
	}
	me := out.Report.Failures[0]
	if me.Class != replay.ClassExtra {
		t.Errorf("Class = %q, want extra", me.Class)
	}
	if me.Stream.Node != fixtureNodeA {
		t.Errorf("Stream.Node = %q, want %q", me.Stream.Node, fixtureNodeA)
	}
	if me.Position != 0 {
		t.Errorf("Position = %d, want 0 (a round the recording never made at all)", me.Position)
	}
	if out.Report.Clean() {
		t.Errorf("Report.Clean() = true, want false")
	}
	// The judge itself DID diverge structurally (out.Err, when non-nil, is
	// the SAME MissError the report already captured) - either shape is
	// acceptable proof; the report is the one that's guaranteed.
	if out.Err != nil {
		var wrapped *replay.MissError
		if !errors.As(out.Err, &wrapped) {
			t.Errorf("out.Err = %v (%T), want it to wrap a *replay.MissError when non-nil", out.Err, out.Err)
		}
	}
}
