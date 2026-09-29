package dag

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// blockModel streams one partial chunk (so the node is running), then blocks until ctx ends.
type blockModel struct{ started chan struct{} }

func (blockModel) Name() string { return "block" }

func (m blockModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if !yield(&model.LLMResponse{Content: genai.NewContentFromText("working", genai.RoleModel), Partial: true}, nil) {
			return
		}
		select {
		case m.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// terminalRecorder collects node terminal events by node id.
type terminalRecorder struct {
	mu  sync.Mutex
	got map[string]stream.SSEEvent
}

func (r *terminalRecorder) record(ev stream.SSEEvent) {
	var id string
	switch d := ev.Data.(type) {
	case stream.NodeDoneData:
		id = d.NodeID
	case stream.NodeFailedData:
		id = d.NodeID
	case stream.NodeCancelledData:
		id = d.NodeID
	case stream.NodePausedData:
		id = d.NodeID
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[id] = ev
}

func (r *terminalRecorder) of(id string) (stream.SSEEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev, ok := r.got[id]
	return ev, ok
}

func blockingExecutor(t *testing.T) (*Executor, blockModel) {
	t.Helper()
	m := blockModel{started: make(chan struct{}, 1)}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	return ex, m
}

func waitStarted(t *testing.T, m blockModel) {
	t.Helper()
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("node never started")
	}
}

var chainPlan = Plan{ID: "p", UserMessage: "go", Nodes: []Node{
	{ID: "n1", AgentName: "w", Task: "t1"},
	{ID: "n2", AgentName: "w", Task: "t2", DependsOn: []string{"n1"}},
}}

// TestRunPlanAsGraph_StopSettlesCancelled: a user stop cancels the running node and
// its never-started descendant instead of leaving them running/queued.
func TestRunPlanAsGraph_StopSettlesCancelled(t *testing.T) {
	ex, m := blockingExecutor(t)
	rec := &terminalRecorder{got: map[string]stream.SSEEvent{}}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ex.RunPlanAsGraph(ctx, chainPlan, "quack", "u", "chat", genai.NewContentFromText("go", genai.RoleUser),
			func(ev stream.SSEEvent, _ error) bool { rec.record(ev); return true }, map[string]string{}, nil)
		done <- err
	}()
	waitStarted(t, m)
	stop()
	if err := <-done; err == nil {
		t.Fatal("RunPlanAsGraph err = nil, want the stop's error")
	}
	for _, id := range []string{"n1", "n2"} {
		if ev, ok := rec.of(id); !ok || ev.Name != stream.EventNodeCancelled {
			t.Errorf("%s terminal = %q (ok=%v), want %s", id, ev.Name, ok, stream.EventNodeCancelled)
		}
	}
}

// TestRunPlanStep_TimeoutFailsStartedOnly: a non-stop runner error fails the started
// node with the real reason and leaves the never-dispatched one alone (Started's contract).
func TestRunPlanStep_TimeoutFailsStartedOnly(t *testing.T) {
	ex, _ := blockingExecutor(t)
	rec := &terminalRecorder{got: map[string]stream.SSEEvent{}}
	ctx, cancel := context.WithTimeout(stream.WithYield(context.Background(), rec.record), 500*time.Millisecond)
	defer cancel()
	_, _, started, err := ex.RunPlanStep(ctx, chainPlan, "quack", "u", "chat", nil, map[string]bool{"n1": true, "n2": true})
	if err == nil {
		t.Fatal("RunPlanStep err = nil, want the deadline's error")
	}
	ev, ok := rec.of("n1")
	d, isFail := ev.Data.(stream.NodeFailedData)
	if !ok || !isFail || d.Error != "plan run timed out" {
		t.Errorf("n1 terminal = %+v (ok=%v), want node_failed \"plan run timed out\"", ev, ok)
	}
	if ev, ok := rec.of("n2"); ok || started["n2"] {
		t.Errorf("never-started n2 got terminal %+v (started=%v), want none", ev, started["n2"])
	}
}

// TestAbort_ShutdownLeavesNodesForBoot: a shutdown cut of an unpaused node emits no terminal
// event and writes no terminal ledger entry, so boot re-stamps it paused/shutdown and resumes it.
func TestAbort_ShutdownLeavesNodesForBoot(t *testing.T) {
	ex, m := blockingExecutor(t)
	led := ledgertest.NewMemStore()
	ex.SetWALLedger(led)
	rec := &terminalRecorder{got: map[string]stream.SSEEvent{}}
	ctx, stop := context.WithCancel(stream.WithYield(context.Background(), rec.record))
	done := make(chan error, 1)
	go func() {
		_, _, _, err := ex.RunPlanStep(ctx, chainPlan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
		done <- err
	}()
	waitStarted(t, m)
	ex.MarkShutdown("chat")
	stop()
	<-done
	if ev, ok := rec.of("n1"); ok {
		t.Errorf("n1 got terminal %s on a shutdown cut, want none", ev.Name)
	}
	entries, _ := led.ReadEntries(context.Background(), "chat", 0)
	if len(entries) != 1 || entries[0].Kind != ledger.KindNodeStarted {
		t.Errorf("ledger = %+v, want only %s", entries, ledger.KindNodeStarted)
	}
}

// TestPausedNodeWritesNoTerminalLedgerEntry: a paused node resumes later, so a cut that
// lands on it records no node.failed/node.cancelled, just its node.started.
func TestPausedNodeWritesNoTerminalLedgerEntry(t *testing.T) {
	ex, m := blockingExecutor(t)
	led := ledgertest.NewMemStore()
	ex.SetWALLedger(led)
	ctx, stop := context.WithCancel(stream.WithYield(context.Background(), func(stream.SSEEvent) {}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = ex.RunPlanStep(ctx, chainPlan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
	}()
	waitStarted(t, m)
	if !ex.PauseNode("chat", "n1", PauseShutdown) {
		t.Fatal("PauseNode found no live control")
	}
	stop()
	<-done
	entries, _ := led.ReadEntries(context.Background(), "chat", 0)
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 1 || kinds[0] != ledger.KindNodeStarted {
		t.Fatalf("ledger kinds = %v, want only %s", kinds, ledger.KindNodeStarted)
	}
}

// judgeBlockModel answers the worker's first call, then blocks every later (judge) call until cancelled.
type judgeBlockModel struct {
	calls   *atomic.Int32
	judging chan struct{}
}

func (judgeBlockModel) Name() string { return "judge-block" }

func (m judgeBlockModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls.Add(1) == 1 {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("THE ANSWER", genai.RoleModel), TurnComplete: true, FinishReason: genai.FinishReasonStop}, nil)
			return
		}
		select {
		case m.judging <- struct{}{}:
		default:
		}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// TestStopDuringJudge: a stop that lands mid-judge settles the node cancelled rather than
// delivering its answer unvetted as done; a shutdown cut there leaves it for boot.
func TestStopDuringJudge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shutdown bool
		want     string
	}{{"user stop", false, stream.EventNodeCancelled}, {"shutdown", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			m := judgeBlockModel{calls: &atomic.Int32{}, judging: make(chan struct{}, 1)}
			w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
			if err != nil {
				t.Fatal(err)
			}
			ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
				vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
			rec := &terminalRecorder{got: map[string]stream.SSEEvent{}}
			ctx, stop := context.WithCancel(stream.WithYield(context.Background(), rec.record))
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _, _ = ex.RunPlanStep(ctx, chainPlan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
			}()
			select {
			case <-m.judging:
			case <-time.After(10 * time.Second):
				t.Fatal("judge never started")
			}
			if tc.shutdown {
				ex.MarkShutdown("chat")
			}
			stop()
			<-done
			ev, ok := rec.of("n1")
			if tc.want == "" && ok {
				t.Errorf("n1 terminal = %s, want none (left for boot)", ev.Name)
			}
			if tc.want != "" && (!ok || ev.Name != tc.want) {
				t.Errorf("n1 terminal = %q (ok=%v), want %s", ev.Name, ok, tc.want)
			}
		})
	}
}

// TestCancelledNodeDraftStaysForDependents: a node cancelled after its draft keeps that draft
// as its output (dependents receive it, flagged as not passing review) but reads as stopped,
// which is what keeps it from ever being delivered as the answer.
func TestCancelledNodeDraftStaysForDependents(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)
	go func() {
		<-stub.started
		ex.CancelNode("chat", "n1")
		close(stub.unblock)
	}()
	rec := &terminalRecorder{got: map[string]stream.SSEEvent{}}
	outputs, _, _, err := ex.RunPlanStep(stream.WithYield(context.Background(), rec.record), plan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
	if err != nil {
		t.Fatal(err)
	}
	if outputs["n1"] != "draft" || !ex.NodeStopped("chat", "n1") {
		t.Errorf("outputs[n1] = %q stopped=%v, want the draft kept and the node stopped", outputs["n1"], ex.NodeStopped("chat", "n1"))
	}
	if ev, ok := rec.of("n1"); !ok || ev.Name != stream.EventNodeCancelled {
		t.Errorf("n1 terminal = %+v (ok=%v), want node_cancelled", ev, ok)
	}
}
