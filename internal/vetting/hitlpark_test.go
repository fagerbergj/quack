package vetting

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/adk/v2/workflow"
)

// parkCtrl records what the HITL park told the node state machine.
type parkCtrl struct{ question string }

func (p *parkCtrl) Cancelled() bool        { return false }
func (p *parkCtrl) Paused() bool           { return p.question != "" }
func (p *parkCtrl) TakeQueued() string     { return "" }
func (p *parkCtrl) PauseForInput(q string) { p.question = q }

// TestParkForInput: a worker question folds into the one pause path -
// markPaused(awaiting_input) with the question - and returns ErrNodePaused,
// the single sentinel quack code checks. ADK's own ErrNodeInterrupted stays
// in the chain because the engine keys the park off it.
func TestParkForInput(t *testing.T) {
	ctrl := &parkCtrl{}
	err := parkForInput(ctrl, "which region?", workflow.ErrNodeInterrupted)

	if ctrl.question != "which region?" {
		t.Errorf("control question = %q; want the worker's question", ctrl.question)
	}
	if !errors.Is(err, ErrNodePaused) {
		t.Errorf("err = %v; want ErrNodePaused", err)
	}
	if !errors.Is(err, workflow.ErrNodeInterrupted) {
		t.Errorf("err = %v; ADK's park sentinel must stay in the chain", err)
	}
	if !isReviewerPauseSentinel(err) {
		t.Error("a HITL park must not count as a failed reviewer sibling")
	}
}

// An emit failure is not a park: no pause, no ErrNodePaused.
func TestParkForInput_EmitFailurePassesThrough(t *testing.T) {
	ctrl := &parkCtrl{}
	boom := fmt.Errorf("emit: boom")
	if err := parkForInput(ctrl, "q", boom); !errors.Is(err, boom) || errors.Is(err, ErrNodePaused) {
		t.Errorf("err = %v; want the raw emit failure", err)
	}
	if ctrl.question != "" {
		t.Error("an emit failure must not park the node")
	}
}
