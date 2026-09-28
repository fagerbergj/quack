package dag

import (
	"context"
	"iter"
	"sync"
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

// TestAbort_ShutdownLeavesNodesForBoot: a shutdown cut emits no terminal event, so the
// row stays running and boot re-stamps it paused/shutdown and resumes it.
func TestAbort_ShutdownLeavesNodesForBoot(t *testing.T) {
	ex, m := blockingExecutor(t)
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
