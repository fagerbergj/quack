package dag

import (
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// A queued revise round's run id carries a "-s%d" suffix that must not break parsing,
// or revise rounds report as worker round 0.
func TestStageRound_QueuedReviseKeepsItsStage(t *testing.T) {
	for _, tc := range []struct {
		runID string
		stage string
		round int
	}{
		{"worker-r0", stream.StageWorker, 0},
		{"worker-r1", stream.StageRevise, 1},
		{"worker-r2", stream.StageRevise, 2},
		{"worker-r0-s3", stream.StageWorker, 0},
		{"worker-r1-s2", stream.StageRevise, 1},
		{"worker-r12-s7", stream.StageRevise, 12},
		{"worker-hitl-r1-s2", stream.StageWorker, 0},
		{"worker-cont1-s2", stream.StageWorker, 0},
		{"worker-r1-sx", stream.StageWorker, 0}, // not a queue suffix, don't trim
	} {
		gotStage, gotRound := stageRound(tc.runID)
		if gotStage != tc.stage || gotRound != tc.round {
			t.Errorf("stageRound(%q) = (%q, %d), want (%q, %d)", tc.runID, gotStage, gotRound, tc.stage, tc.round)
		}
	}
}
