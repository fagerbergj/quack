package dag

import (
	"context"
	"iter"
	"sync"
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

// A cap of 1 shared between the orchestrator and its nodes must not deadlock:
// this is the config that made the naive whole-run approach hang.
func TestAdmittingLLMDoesNotDeadlockNodesOnSameModel(t *testing.T) {
	spec := AdmissionSpec{Model: "m"}
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, 0)
	f := &fakeLLM{entered: make(chan struct{}, 8), release: make(chan struct{})}
	llm := NewAdmittingLLM(f, a, spec, nil)
	close(f.release) // turns complete immediately

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// One orchestrator turn, then the node it "planned" - the
			// sequence that hung when the turn held its session throughout.
			drainLLM(llm.GenerateContent(context.Background(), nil, false))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if !a.Admit(ctx, spec, func() {}) {
				t.Error("planned node never admitted - orchestrator starved its own DAG")
				return
			}
			a.Release(spec)
		}()
	}
	wg.Wait()
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
