package dag

import (
	"context"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
)

// fakeLLM blocks inside GenerateContent until released, so a test can observe
// what capacity is held while a turn is mid-flight.
type fakeLLM struct {
	entered chan struct{}
	release chan struct{}
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.entered <- struct{}{}
		<-f.release
		yield(&model.LLMResponse{}, nil)
	}
}

func drainLLM(seq iter.Seq2[*model.LLMResponse, error]) {
	for range seq { //nolint:revive // consuming for effect
	}
}

// The whole point of the wrapper: a session is held only while generating, so
// an orchestrator parked waiting on its worker nodes holds nothing.
func TestAdmittingLLMReleasesBetweenTurns(t *testing.T) {
	spec := AdmissionSpec{Model: "m"}
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	f := &fakeLLM{entered: make(chan struct{}), release: make(chan struct{})}
	llm := NewAdmittingLLM(f, a, spec, nil)

	done := make(chan struct{})
	go func() { defer close(done); drainLLM(llm.GenerateContent(context.Background(), nil, false)) }()
	<-f.entered

	// Mid-turn the single session is taken: a node on the same model waits.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if a.Admit(ctx, spec, func() {}) {
		a.Release(spec)
		t.Fatal("a node was admitted while the orchestrator turn held the only session")
	}

	close(f.release)
	<-done

	// Turn over: the session must be back, or a parked orchestrator would
	// starve the very nodes it planned.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if !a.Admit(ctx2, spec, func() {}) {
		t.Fatal("session not released after the turn finished")
	}
	a.Release(spec)
}

// A cap of 1 shared between the orchestrator and its nodes must not deadlock.
// The turn must be mid-flight when the node asks, or this proves nothing: that
// overlap is exactly what a run-scoped reservation would never release.
func TestAdmittingLLMDoesNotDeadlockNodesOnSameModel(t *testing.T) {
	spec := AdmissionSpec{Model: "m"}
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	f := &fakeLLM{entered: make(chan struct{}), release: make(chan struct{})}
	llm := NewAdmittingLLM(f, a, spec, nil)

	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		drainLLM(llm.GenerateContent(context.Background(), nil, false))
	}()
	<-f.entered // the turn now holds the only session

	nodeAdmitted := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok := a.Admit(ctx, spec, func() {})
		if ok {
			a.Release(spec)
		}
		nodeAdmitted <- ok
	}()

	close(f.release) // the turn ends, as it would when the orchestrator parks
	<-turnDone
	if !<-nodeAdmitted {
		t.Fatal("node never admitted: the orchestrator starved the DAG it planned")
	}
}

func TestNewAdmittingLLMUnwrapsWhenUnenforced(t *testing.T) {
	f := &fakeLLM{}
	if got := NewAdmittingLLM(f, nil, AdmissionSpec{Model: "m"}, nil); got != model.LLM(f) {
		t.Error("nil admission should return the inner LLM unwrapped")
	}
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	if got := NewAdmittingLLM(f, a, AdmissionSpec{}, nil); got != model.LLM(f) {
		t.Error("an unregistered orchestrator model should return the inner LLM unwrapped")
	}
}
