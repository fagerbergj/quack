package dag

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// blockingJudgeStub answers its worker call instantly but blocks its judge
// call on release, closing judging the first time it's entered - lets a test
// hold a node in its judge phase while checking what capacity is free.
type blockingJudgeStub struct {
	judging chan struct{}
	release chan struct{}
	score   float64
	once    sync.Once
}

func (s *blockingJudgeStub) Name() string { return "blocking-judge-stub" }

func (s *blockingJudgeStub) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			s.once.Do(func() { close(s.judging) })
			select {
			case <-s.release:
			case <-ctx.Done():
			}
			yield(gCall("submit_verdict", map[string]any{"score": s.score}), nil)
			return
		}
		yield(gText("ANSWER"), nil)
	}
}

// signalingStub answers instantly, closing workerStarted the first time its
// (non-judge) worker call runs.
type signalingStub struct {
	workerStarted chan struct{}
	once          sync.Once
}

func (s *signalingStub) Name() string { return "signaling-stub" }

func (s *signalingStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9}), nil)
			return
		}
		s.once.Do(func() { close(s.workerStarted) })
		yield(gText("ANSWER"), nil)
	}
}

// TestJudgePhaseFreesWorkerSlotForSecondNode proves node1's worker spec is
// released before its judge call, so node2 admits onto the same pool-of-one spec while node1 is still judging (the old whole-lifetime bracket would deadlock this).
func TestJudgePhaseFreesWorkerSlotForSecondNode(t *testing.T) {
	admission := NewAdmission(map[string]int{"w": 1}, nil, nil, 0)
	workerSpec := AdmissionSpec{Model: "w"}
	judgeSpec := AdmissionSpec{Model: "j"}
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 1}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w1"}, {ID: "n2", AgentName: "w2"}}}

	blocking := &blockingJudgeStub{judging: make(chan struct{}), release: make(chan struct{}), score: 0.9}
	ag1, _ := llmagent.New(llmagent.Config{Name: "w1", Model: blocking, Description: "w", Instruction: "ROLE:w Answer."})
	wn1, _ := vetting.NewWorkerNode(ag1)
	gn1 := newGatedNode(plan, plan.Nodes[0], wn1, nil, nil, nil, vetting.NewJudgeFactory(blocking, nil, nil), cfg, nil, nil, "", nil, nil, admission, workerSpec, judgeSpec, nil)

	sig := &signalingStub{workerStarted: make(chan struct{})}
	ag2, _ := llmagent.New(llmagent.Config{Name: "w2", Model: sig, Description: "w", Instruction: "ROLE:w Answer."})
	wn2, _ := vetting.NewWorkerNode(ag2)
	gn2 := newGatedNode(plan, plan.Nodes[1], wn2, nil, nil, nil, vetting.NewJudgeFactory(sig, nil, nil), cfg, nil, nil, "", nil, nil, admission, workerSpec, AdmissionSpec{}, nil)

	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = workflow.RunNode[string](ctx, gn1, plan.UserMessage)
			}()

			select {
			case <-blocking.judging:
			case <-time.After(5 * time.Second):
				return "", context.DeadlineExceeded
			}

			// node2 must be admissible right now: node1 released its worker
			// spec before entering the judge call above, and hasn't reclaimed it yet.
			if _, err := workflow.RunNode[string](ctx, gn2, plan.UserMessage); err != nil {
				return "", err
			}
			select {
			case <-sig.workerStarted:
			default:
				t.Error("node2's worker never ran while node1 was still judging")
			}

			close(blocking.release)
			wg.Wait()
			return "done", nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if used, _, _ := admission.Usage(workerSpec); used != 0 {
		t.Errorf("worker spec usage after both nodes finished = %d, want 0", used)
	}
}

// TestJudgePhaseCancelReleasesExactlyOnce cancels a node mid-judge-call and
// checks the ledger returns to zero usage on both specs - no leak, no double release.
func TestJudgePhaseCancelReleasesExactlyOnce(t *testing.T) {
	admission := NewAdmission(map[string]int{"w": 1, "j": 1}, nil, nil, 0)
	workerSpec := AdmissionSpec{Model: "w"}
	judgeSpec := AdmissionSpec{Model: "j"}
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 1}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w1"}}}

	blocking := &blockingJudgeStub{judging: make(chan struct{}), release: make(chan struct{}), score: 0.9}
	ag1, _ := llmagent.New(llmagent.Config{Name: "w1", Model: blocking, Description: "w", Instruction: "ROLE:w Answer."})
	wn1, _ := vetting.NewWorkerNode(ag1)
	gn1 := newGatedNode(plan, plan.Nodes[0], wn1, nil, nil, nil, vetting.NewJudgeFactory(blocking, nil, nil), cfg, nil, nil, "", nil, nil, admission, workerSpec, judgeSpec, nil)

	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			cctx, cancel := ctx.WithAgentCancel()
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = workflow.RunNode[string](cctx, gn1, plan.UserMessage)
			}()

			select {
			case <-blocking.judging:
			case <-time.After(5 * time.Second):
				return "", context.DeadlineExceeded
			}
			cancel()
			// Unblock the judge call too, so the node can actually finish
			// tearing down instead of leaking the goroutine past the test.
			close(blocking.release)
			wg.Wait()
			return "done", nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if used, _, _ := admission.Usage(workerSpec); used != 0 {
		t.Errorf("worker spec usage after cancel = %d, want 0", used)
	}
	if used, _, _ := admission.Usage(judgeSpec); used != 0 {
		t.Errorf("judge spec usage after cancel = %d, want 0", used)
	}
}

// TestJudgePhasePauseReleasesExactlyOnce pauses a node right after a failed
// judge round re-admits the worker spec for a revise, and checks the ledger returns to zero on both specs.
func TestJudgePhasePauseReleasesExactlyOnce(t *testing.T) {
	admission := NewAdmission(map[string]int{"w": 1, "j": 1}, nil, nil, 0)
	workerSpec := AdmissionSpec{Model: "w"}
	judgeSpec := AdmissionSpec{Model: "j"}
	// score 0.4 < Threshold fails every round; JudgeRounds:2 keeps round 1 non-terminal.
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 2}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w1"}}}
	controls := newRunControls()
	const chatID = "c1"

	blocking := &blockingJudgeStub{judging: make(chan struct{}), release: make(chan struct{}), score: 0.4}
	ag1, _ := llmagent.New(llmagent.Config{Name: "w1", Model: blocking, Description: "w", Instruction: "ROLE:w Answer."})
	wn1, _ := vetting.NewWorkerNode(ag1)
	gn1 := newGatedNode(plan, plan.Nodes[0], wn1, nil, nil, nil, vetting.NewJudgeFactory(blocking, nil, nil), cfg, nil, controls, chatID, nil, nil, admission, workerSpec, judgeSpec, nil)

	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = workflow.RunNode[string](ctx, gn1, plan.UserMessage)
			}()

			select {
			case <-blocking.judging:
			case <-time.After(5 * time.Second):
				return "", context.DeadlineExceeded
			}
			var nc *nodeControl
			for i := 0; i < 500; i++ {
				if nc = controls.get(chatID, plan.Nodes[0].ID); nc != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if nc == nil {
				return "", context.DeadlineExceeded
			}
			nc.PauseForInput("continue?")
			close(blocking.release)
			wg.Wait()
			return "done", nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if used, _, _ := admission.Usage(workerSpec); used != 0 {
		t.Errorf("worker spec usage after pause = %d, want 0", used)
	}
	if used, _, _ := admission.Usage(judgeSpec); used != 0 {
		t.Errorf("judge spec usage after pause = %d, want 0", used)
	}
}

// TestJudgePhaseNoDeadlockOnSimultaneousTransition runs more nodes than the
// pool holds through the worker-to-judge swap at once; a stuck reacquire fails this via -timeout, not an assertion.
func TestJudgePhaseNoDeadlockOnSimultaneousTransition(t *testing.T) {
	const nodeCount = 6
	admission := NewAdmission(map[string]int{"w": 2, "j": 2}, nil, nil, 0)
	workerSpec := AdmissionSpec{Model: "w"}
	judgeSpec := AdmissionSpec{Model: "j"}
	cfg := vetting.Config{Threshold: 0.6, JudgeRounds: 1}

	var nodes []Node
	for i := 0; i < nodeCount; i++ {
		nodes = append(nodes, Node{ID: idFor(i), AgentName: idFor(i)})
	}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: nodes}

	gates := make([]workflow.Node, nodeCount)
	for i := range nodes {
		stub := okStub{}
		ag, _ := llmagent.New(llmagent.Config{Name: nodes[i].AgentName, Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
		wn, _ := vetting.NewWorkerNode(ag)
		gates[i] = newGatedNode(plan, nodes[i], wn, nil, nil, nil, vetting.NewJudgeFactory(stub, nil, nil), cfg, nil, nil, "", nil, nil, admission, workerSpec, judgeSpec, nil)
	}

	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			var wg sync.WaitGroup
			errs := make([]error, nodeCount)
			wg.Add(nodeCount)
			for i := range gates {
				i := i
				go func() {
					defer wg.Done()
					_, errs[i] = workflow.RunNode[string](ctx, gates[i], plan.UserMessage)
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return "", err
				}
			}
			return "done", nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if used, _, _ := admission.Usage(workerSpec); used != 0 {
		t.Errorf("worker spec usage after all nodes finished = %d, want 0", used)
	}
	if used, _, _ := admission.Usage(judgeSpec); used != 0 {
		t.Errorf("judge spec usage after all nodes finished = %d, want 0", used)
	}
}

func idFor(i int) string { return string(rune('a' + i)) }

// TestSetupAdmissionReQueuesOnMidRunWait proves the #1480 fix: a node
// already running that blocks re-acquiring its slot (the worker/judge swap)
// fires node_queued, then node_admitted once let back in - both legal moves
// against dag.CanTransition, not just SSE noise nothing acts on.
func TestSetupAdmissionReQueuesOnMidRunWait(t *testing.T) {
	admission := NewAdmission(map[string]int{"w": 1}, nil, nil, 0)
	spec := AdmissionSpec{Model: "w"}

	holderReady := make(chan struct{})
	releaseHolder := make(chan struct{})
	go func() {
		admission.Admit(context.Background(), spec, nil)
		close(holderReady)
		<-releaseHolder
		admission.Release(spec)
	}()
	<-holderReady

	var mu sync.Mutex
	status := StatusRunning // simulates a node already past its first admission
	var events []string
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev.Name)
		var to NodeStatus
		switch ev.Name {
		case stream.EventNodeQueued:
			to = StatusQueued
		case stream.EventNodeAdmitted:
			to = StatusRunning
		default:
			return
		}
		if !CanTransition(status, to) {
			t.Errorf("illegal transition %s -> %s", status, to)
		}
		status = to
	})

	cfg := &vetting.Config{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		free, err := setupAdmission(ctx, "n1", cfg, admission, spec, AdmissionSpec{})
		if err != nil {
			t.Errorf("setupAdmission: %v", err)
			return
		}
		free()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := status
		mu.Unlock()
		if got == StatusQueued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node never persisted queued while blocked on admission")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(releaseHolder)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if status != StatusRunning {
		t.Errorf("status after re-admission = %s, want running", status)
	}
	if len(events) != 2 || events[0] != stream.EventNodeQueued || events[1] != stream.EventNodeAdmitted {
		t.Errorf("events = %v, want [%s %s]", events, stream.EventNodeQueued, stream.EventNodeAdmitted)
	}
}
