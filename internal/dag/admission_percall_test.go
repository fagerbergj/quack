package dag

import (
	"context"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/vetting"
)

// toolPhaseModel: a fakeLLM that fires toolPhase the moment its single
// (complete) response is yielded - the caller's synchronous tool run, where
// the GPU holds nothing and the per-call hold has just been released.
type toolPhaseModel struct {
	fakeLLM
	toolPhase func()
}

func (m *toolPhaseModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	inner := m.fakeLLM.GenerateContent(ctx, req, stream)
	return func(yield func(*model.LLMResponse, error) bool) {
		inner(func(r *model.LLMResponse, err error) bool {
			if r != nil && !r.Partial && m.toolPhase != nil {
				m.toolPhase()
			}
			return yield(r, err)
		})
	}
}

// TestPerCallHoldsOverlapToolPhases is #1482's concurrency proof: on a
// one-slot kv pool, a native node's slot frees between model calls, so a
// second node's model call admits into the first node's tool phase. A
// whole-run hold deadlocks this: B would wait out A's entire run.
func TestPerCallHoldsOverlapToolPhases(t *testing.T) {
	t.Parallel()
	spec := AdmissionSpec{Model: "w", KVTokens: 1}
	a := NewAdmission(nil, map[string]int{"w": 1}, nil, 0)

	// buffered release: the hold frees the slot before the response yield,
	// so the fake may already be past <-release by the time a later call
	// closes it; an unbuffered close there would deadlock the test.
	aGen := &toolPhaseModel{fakeLLM: fakeLLM{entered: make(chan struct{}), release: make(chan struct{}, 1)}}
	bGen := &toolPhaseModel{fakeLLM: fakeLLM{entered: make(chan struct{}), release: make(chan struct{}, 1)}}

	aTool := make(chan struct{}) // A's complete response yielded: its tool phase began
	aGen.toolPhase = func() { close(aTool) }
	bTool := make(chan struct{})
	bGen.toolPhase = func() { close(bTool) }

	startCall := func(m *toolPhaseModel) {
		llm := NewAdmittingLLM(m, a, spec, nil, nil)
		go drainLLM(llm.GenerateContent(context.Background(), nil, false))
	}

	// A generates; wait until its call is inside the pool. The per-call
	// hold is live until the response is yielded, so the slot is A's now.
	startCall(aGen)
	<-aGen.entered

	// B starts a call that cannot fit while A's hold is live.
	startCall(bGen)
	select {
	case <-bGen.entered:
		t.Fatal("B admitted while A's model call held the only slot")
	case <-time.After(100 * time.Millisecond):
		// B is blocked on admission while A holds the slot - as intended.
	}

	// A's call completes: the hold released before the yield, so the slot
	// is free the moment A enters its tool phase.
	close(aGen.release)
	<-aTool

	// B's blocked call now admits into A's tool phase and runs to
	// completion - the overlap a whole-run hold would never allow.
	<-bGen.entered
	close(bGen.release)
	<-bTool
}

// TestPerCallNoOpHooksSurviveWiring pins the #1519 round-2 fix: with
// perCall=true the no-op ReleaseWorker/AdmitWorker must survive the
// judge-hook wiring below them, else a gated native worker re-holds its
// whole-run slot after the first judge round and self-deadlocks.
func TestPerCallNoOpHooksSurviveWiring(t *testing.T) {
	admission := NewAdmission(map[string]int{"w": 1}, nil, nil, 0)
	spec := AdmissionSpec{Model: "w"}
	cfg := &vetting.Config{}
	free, err := setupAdmission(context.Background(), "n1", cfg, admission, spec, AdmissionSpec{}, true)
	if err != nil {
		t.Fatalf("setupAdmission(perCall=true): %v", err)
	}
	free()
	// pre-fix, AdmitWorker was re-assigned unconditionally and re-hold
	// spec; a full pool then blocks. The no-op must admit instantly.
	done := make(chan bool, 1)
	go func() { done <- cfg.AdmitWorker(context.Background()) }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("perCall AdmitWorker returned false; want the no-op true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("perCall AdmitWorker blocked re-holding the whole-run slot")
	}
	// judge hooks still wired for both modes
	if !cfg.AdmitJudge(context.Background()) {
		t.Error("perCall AdmitJudge failed")
	}
	cfg.ReleaseWorker()
}
