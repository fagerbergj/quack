package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// stopModel streams one partial chunk (so the node is running), then blocks until cancelled.
type stopModel struct{ started chan struct{} }

func (stopModel) Name() string { return "stop" }

func (m stopModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
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

// TestRetryNode_StopSettlesCancelled: stopping a retry mid-node settles the node cancelled.
func TestRetryNode_StopSettlesCancelled(t *testing.T) {
	m := stopModel{started: make(chan struct{}, 1)}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.started:
			stop()
		case <-time.After(10 * time.Second):
		}
	}()
	var cancelled bool
	for ev := range orch.RetryNode(ctx, "u", "chat", "p", nil, "n1", "") {
		if d, ok := ev.Data.(stream.NodeCancelledData); ok && d.NodeID == "n1" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Error("n1 was not settled cancelled after the stop")
	}
}

// TestRetryNode_RefusesStaleStash: a stash holding another plan (a run cut mid-execute)
// must not run that plan's task under the node's id, nor announce it as the chat's plan.
func TestRetryNode_RefusesStaleStash(t *testing.T) {
	sessions := session.InMemoryService()
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	planJSON, _ := json.Marshal(dag.Plan{ID: "old", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "old task"}}})
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for ev := range orch.RetryNode(context.Background(), "u", "chat", "new", nil, "n1", "") {
		names = append(names, ev.Name)
	}
	if len(names) != 1 || names[0] != stream.EventError {
		t.Fatalf("events = %v, want exactly one error", names)
	}
}

// TestFinalizeAnswer_StoppedTerminalIsNoAnswer: once the user stopped the terminal node,
// every delivery path's finalize yields nothing, whatever draft its outputs hold.
func TestFinalizeAnswer_StoppedTerminalIsNoAnswer(t *testing.T) {
	m := stopModel{started: make(chan struct{}, 1)}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	go func() {
		<-m.started
		ex.CancelNode("chat", "n1")
	}()
	_, _, _, _ = ex.RunPlanStep(stream.WithYield(context.Background(), func(stream.SSEEvent) {}), plan, AppName, "u", "chat", nil, map[string]bool{"n1": true})
	if got := orch.finalizeAnswer(context.Background(), plan, map[string]string{"n1": "DRAFT"}, "chat", nil); got != "" {
		t.Errorf("finalizeAnswer = %q, want no answer for a stopped terminal node", got)
	}
}

// TestPersistAnswerMarksDeliveredAnswer: the delivered answer is marked with its turn, so a
// reload attaches it there even when a retry appends it after later turns.
func TestPersistAnswerMarksDeliveredAnswer(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions}
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatal(err)
	}
	o.persistAnswer(stream.WithTurnID(context.Background(), "t1"), "u", "c", "THE ANSWER")
	resp, err := sessions.Get(context.Background(), &session.GetRequest{AppName: AppName, UserID: "u", SessionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range resp.Session.Events().All() {
		if turn, _ := ev.CustomMetadata[stream.DeliveredAnswerMeta].(string); turn == "t1" {
			if at, _ := ev.CustomMetadata[stream.DeliveredAtMeta].(string); at == "" {
				t.Error("the delivered answer carries no delivery time to order retries by")
			}
			return
		}
	}
	t.Fatal("the persisted answer event is not marked delivered")
}

// TestDeliverFromRecord_StoppedTerminalNeverDelivered: a resume in a later turn delivers from the
// plan record; a terminal node stopped in an earlier turn has no stop flag left, only Stopped.
func TestDeliverFromRecord_StoppedTerminalNeverDelivered(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "t", AgentName: "w", DependsOn: []string{"h"}}, {ID: "h", AgentName: "w"}}}
	for _, stopped := range []bool{true, false} {
		o.deliverFromRecord(stream.WithTurnID(context.Background(), "t1"), "u", "c", plan, dag.DagPlanRecord{
			PlanID: "p", Assignments: []dag.Assignment{{NodeID: "t", Result: "TERMINAL OUT", Stopped: stopped}, {NodeID: "h", Result: "RESUMED OUT"}},
		})
		got := o.LatestAnswer(context.Background(), "u", "c")
		if stopped && got != "" {
			t.Fatalf("delivered %q from a stopped terminal", got)
		}
		if !stopped && got != "TERMINAL OUT" {
			t.Fatalf("delivered %q, want the reviewed terminal output", got)
		}
	}
}

// TestDeliverFromRecord_MultiSink: every sink ships as its own section; one stopped in an earlier
// turn ships only a note, and with every sink stopped nothing is delivered.
func TestDeliverFromRecord_MultiSink(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "r1", AgentName: "w"}, {ID: "r2", AgentName: "w"}}}
	cases := []struct {
		stopped [2]bool
		want    string
	}{
		{[2]bool{false, false}, "## r1\n\nONE\n\n## r2\n\nTWO"},
		{[2]bool{false, true}, "## r1\n\nONE\n\n## r2\n\n" + stream.StoppedSinkNote},
		{[2]bool{true, true}, ""},
	}
	for i, tc := range cases {
		chat := fmt.Sprintf("c%d", i)
		if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: chat}); err != nil {
			t.Fatal(err)
		}
		o.deliverFromRecord(stream.WithTurnID(context.Background(), "t1"), "u", chat, plan, dag.DagPlanRecord{
			PlanID: "p", Assignments: []dag.Assignment{{NodeID: "r1", Result: "ONE", Stopped: tc.stopped[0]}, {NodeID: "r2", Result: "TWO", Stopped: tc.stopped[1]}},
		})
		if got := o.LatestAnswer(context.Background(), "u", chat); got != tc.want {
			t.Errorf("stopped=%v: delivered %q, want %q", tc.stopped, got, tc.want)
		}
	}
}

// TestWithRecordedSinks_RetryKeepsSiblingSinks: retrying one sink of two delivers both - the
// sibling from the plan record, its stopped draft masked - while a single-sink plan is untouched.
func TestWithRecordedSinks_RetryKeepsSiblingSinks(t *testing.T) {
	sessions := session.InMemoryService()
	svc := artifact.InMemoryService()
	o := &Orchestrator{sessions: sessions, artifacts: svc, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := context.Background()
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "c", "", dag.DagPlanRecord{
		PlanID: "p", Assignments: []dag.Assignment{{NodeID: "a", Task: "a", Result: "OLD A"}, {NodeID: "b", Task: "b", Result: "B DRAFT", Stopped: true}},
	}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a"}, {ID: "b"}}}
	// The retry's seeds carry b's raw draft; only a actually ran.
	retried := map[string]string{"a": "NEW A", "b": "B DRAFT"}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, retried, func(id string) bool { return id == "a" })
	want := "## a\n\nNEW A\n\n## b\n\n" + stream.StoppedSinkNote
	if got := o.finalizeAnswer(ctx, plan, outputs, "c", recStopped); got != want {
		t.Errorf("finalizeAnswer = %q, want %q", got, want)
	}
	single := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}}}
	if got, _ := o.withRecordedSinks(ctx, "u", "c", single, retried, func(string) bool { return false }); !maps.Equal(got, retried) {
		t.Errorf("single-sink outputs = %v, want them unchanged", got)
	}
}

// TestWithRecordedSinks_NoRecordMasksUnreviewedSeed: retrying in a plan the record no longer
// describes still masks a stopped sibling, from the seeds' unreviewed flags.
func TestWithRecordedSinks_NoRecordMasksUnreviewedSeed(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions, artifacts: artifact.InMemoryService(), executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := dag.WithUnreviewedSeeds(context.Background(), map[string]bool{"b": true})
	plan := dag.Plan{ID: "older", Nodes: []dag.Node{{ID: "a"}, {ID: "b"}}}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, map[string]string{"a": "NEW A", "b": "B STOPPED DRAFT"}, func(id string) bool { return id == "a" })
	want := "## a\n\nNEW A\n\n## b\n\n" + stream.StoppedSinkNote
	if got := o.finalizeAnswer(ctx, plan, outputs, "c", recStopped); got != want {
		t.Errorf("finalizeAnswer = %q, want %q", got, want)
	}
}

// TestWithRecordedSinks_ExtendedPlanKeepsItsStepSinks: turn 1 ran r1 and r2, turn 2 added r3; a
// retry of r3 answers with r3 alone, the sinks the record says that step delivers.
func TestWithRecordedSinks_ExtendedPlanKeepsItsStepSinks(t *testing.T) {
	sessions := session.InMemoryService()
	svc := artifact.InMemoryService()
	// A model that would reword: retry and resume deliver the node's own output, never a format pass.
	o := &Orchestrator{sessions: sessions, artifacts: svc, model: answerModel{text: "REFORMATTED"}, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := context.Background()
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "c", "", dag.DagPlanRecord{
		PlanID: "p", Sinks: []string{"r3"},
		Assignments: []dag.Assignment{{NodeID: "r1", Task: "a", Result: "R1"}, {NodeID: "r2", Task: "b", Result: "R2"}, {NodeID: "r3", Task: "c", Result: "OLD R3"}},
	}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "r1"}, {ID: "r2"}, {ID: "r3"}}}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, map[string]string{"r1": "R1", "r2": "R2", "r3": "NEW R3"}, func(id string) bool { return id == "r3" })
	if got, _ := o.sinkAnswer(plan, outputs, "c", recStopped); got != "NEW R3" {
		t.Errorf("finalizeAnswer = %q, want only this step's sink", got)
	}
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatal(err)
	}
	o.deliverFromRecord(stream.WithTurnID(ctx, "t2"), "u", "c", plan, dag.DagPlanRecord{PlanID: "p", Sinks: []string{"r3"},
		Assignments: []dag.Assignment{{NodeID: "r1", Result: "R1"}, {NodeID: "r2", Result: "R2"}, {NodeID: "r3", Result: "RESUMED R3"}}})
	if got := o.LatestAnswer(ctx, "u", "c"); got != "RESUMED R3" {
		t.Errorf("resume delivered %q, want only this step's sink", got)
	}
}

// TestFinalizeAnswer_SectionsSkipFormatPass: a long multi-sink answer keeps its sections verbatim;
// the format pass could merge them into the synthesis the plan left out.
func TestFinalizeAnswer_SectionsSkipFormatPass(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions, model: answerModel{text: "REFORMATTED"}, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	long := strings.Repeat("finding ", 400)
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a", AgentName: "w"}, {ID: "b", AgentName: "w"}}}
	got := o.finalizeAnswer(context.Background(), plan, map[string]string{"a": long, "b": long}, "c", nil)
	if want := "## a\n\n" + strings.TrimSpace(long) + "\n\n## b\n\n" + strings.TrimSpace(long); got != want {
		t.Errorf("finalizeAnswer reformatted a %d-char sectioned answer: %.60q", len(want), got)
	}
	single := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a", AgentName: "w"}}}
	if got := o.finalizeAnswer(context.Background(), single, map[string]string{"a": long}, "c", nil); got != "REFORMATTED" {
		t.Errorf("single long sink = %.60q, want the format pass as before", got)
	}
}

// answerModel answers every call with its text.
type answerModel struct{ text string }

func (answerModel) Name() string { return "answer" }

func (m answerModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText(m.text, genai.RoleModel), TurnComplete: true}, nil)
	}
}

// TestRetryNode_SettlesStoppedAssignment: an explicit retry of a stopped node that succeeds
// clears the assignment's stop and records the fresh output, so later deliveries and seeds use it.
func TestRetryNode_SettlesStoppedAssignment(t *testing.T) {
	m := answerModel{text: "FRESH"}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	svc := artifact.InMemoryService()
	orch.SetArtifacts(svc)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	planJSON, _ := json.Marshal(plan)
	ctx := context.Background()
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "chat", "", dag.DagPlanRecord{
		PlanID: "p", Assignments: []dag.Assignment{{NodeID: "n1", Task: "t", TaskID: "t-1", Result: "OLD DRAFT", Stopped: true}},
	}); err != nil {
		t.Fatal(err)
	}
	for range orch.RetryNode(ctx, "u", "chat", "p", nil, "n1", "") {
	}
	rec, _, _, err := dag.LoadDagPlanRecord(ctx, svc, artifactref.AppName, "u", "chat")
	if err != nil || len(rec.Assignments) != 1 || rec.Assignments[0].Stopped || rec.Assignments[0].Result != "FRESH" {
		t.Fatalf("record = %+v err=%v, want n1 unstopped with the retry's output", rec, err)
	}
}

// taskModel answers "FRESH A" for node a's task and "FRESH C" for any other.
type taskModel struct{}

func (taskModel) Name() string { return "task" }

func (taskModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	text := "FRESH C"
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p != nil && strings.Contains(p.Text, "TASK-A") {
				text = "FRESH A"
			}
		}
	}
	return answerModel{text: text}.GenerateContent(context.Background(), req, false)
}

// extension is plan a, b, c (c after b): turn 1 ran a and b, turn 2 added c, and the record says sinks.
type extension struct {
	orch *Orchestrator
	svc  artifact.Service
	ctx  context.Context
}

func newExtension(t *testing.T, sinks []string) *extension {
	t.Helper()
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: taskModel{}, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": taskModel{}},
		vetting.NewJudgeFactory(taskModel{}, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	// The orchestrator's model would reword a formatted answer; a retry delivers the node's output verbatim.
	e := &extension{orch: New(sessions, answerModel{text: "REFORMATTED"}, func(context.Context) string { return "" }, nil, ex, nil, nil, nil),
		svc: artifact.InMemoryService(), ctx: stream.WithTurnID(context.Background(), "turn-2")}
	e.orch.SetArtifacts(e.svc)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{
		{ID: "a", AgentName: "w", Task: "TASK-A"}, {ID: "b", AgentName: "w", Task: "TASK-B"}, {ID: "c", AgentName: "w", Task: "TASK-C", DependsOn: []string{"b"}},
	}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(e.ctx, &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dag.SaveDagPlanRecord(e.ctx, e.svc, artifactref.AppName, "u", "chat", "", dag.DagPlanRecord{
		PlanID: "p", Sinks: sinks, Assignments: []dag.Assignment{
			{NodeID: "a", Task: "TASK-A", TaskID: "t1", Result: "A"}, {NodeID: "b", Task: "TASK-B", TaskID: "t2", Result: "B"},
			{NodeID: "c", Task: "TASK-C", DependsOn: []string{"b"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// retry retries node (as boot resume does) and returns the persisted answer; check sees every event.
func (e *extension) retry(node, guidance string, check func(stream.SSEEvent)) string {
	for ev := range e.orch.RetryNode(e.ctx, "u", "chat", "p", map[string]string{"a": "A", "b": "B"}, node, guidance) {
		check(ev)
	}
	return e.orch.LatestAnswer(e.ctx, "u", "chat")
}

// TestRetryNode_ExtensionDeliversOnlyItsStep: retrying c with guidance persists c's fresh answer alone,
// not turn 1's sinks beside it.
func TestRetryNode_ExtensionDeliversOnlyItsStep(t *testing.T) {
	got := newExtension(t, []string{"c"}).retry("c", "focus on X", func(ev stream.SSEEvent) {
		// The persisted plan keeps c's original task: guidance is this retry's alone.
		if d, ok := ev.Data.(stream.DagPlanData); ok && (d.Nodes[2].Task != "TASK-C" || strings.Contains(string(d.ExecPlan), "focus on X")) {
			t.Errorf("dag_plan event carries the retry guidance: task %q", d.Nodes[2].Task)
		}
	})
	if got != "FRESH C" {
		t.Errorf("retry delivered %q, want only this step's sink", got)
	}
}

// TestRetryNode_StaleRecordSinks: a restart while turn 2's c ran left the record with turn 1's sinks
// [a b]; the boot resume (a retry of c) delivers c alone and settles the record on c's step, so a later
// retry of a follows the normal rule and answers with that step's c.
func TestRetryNode_StaleRecordSinks(t *testing.T) {
	e := newExtension(t, []string{"a", "b"})
	if got := e.retry("c", "", func(stream.SSEEvent) {}); got != "FRESH C" {
		t.Errorf("resume delivered %q, want c's output", got)
	}
	rec, _, _, err := dag.LoadDagPlanRecord(e.ctx, e.svc, artifactref.AppName, "u", "chat")
	if err != nil || !slices.Equal(rec.Sinks, []string{"c"}) || rec.Assignments[2].TaskID == "" || rec.Assignments[2].Result != "FRESH C" {
		t.Fatalf("record = %+v err=%v, want sinks [c] and c settled", rec, err)
	}
	e.orch.persistAnswer(e.ctx, "u", "chat", "MARKER") // so the next check reads the retry's own delivery
	if got := e.retry("a", "", func(stream.SSEEvent) {}); got != "FRESH C" {
		t.Errorf("retry of a delivered %q, want the latest step's c from the record", got)
	}
	if rec, _, _, _ := dag.LoadDagPlanRecord(e.ctx, e.svc, artifactref.AppName, "u", "chat"); rec.Assignments[0].Result != "FRESH A" {
		t.Errorf("a's result = %q, want the retry's fresh output", rec.Assignments[0].Result)
	}
}

// TestWithRecordedSinks_RetryOfEarlierSinkKeepsRecord: retrying turn 1's a after turn 2's c settled is
// no stale record - the answer stays the latest step's, c.
func TestWithRecordedSinks_RetryOfEarlierSinkKeepsRecord(t *testing.T) {
	sessions := session.InMemoryService()
	svc := artifact.InMemoryService()
	o := &Orchestrator{sessions: sessions, artifacts: svc, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := context.Background()
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "c", "", dag.DagPlanRecord{
		PlanID: "p", Sinks: []string{"c"},
		Assignments: []dag.Assignment{{NodeID: "a", Task: "ta", TaskID: "t1", Result: "A"}, {NodeID: "b", Task: "tb", TaskID: "t2", Result: "B"}, {NodeID: "c", Task: "tc", TaskID: "t3", Result: "C", DependsOn: []string{"b"}}},
	}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a"}, {ID: "b"}, {ID: "c", DependsOn: []string{"b"}}}}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, map[string]string{"a": "NEW A", "b": "B", "c": "C"}, func(id string) bool { return id == "a" })
	if got, _ := o.sinkAnswer(plan, outputs, "c", recStopped); got != "C" {
		t.Errorf("retry of a delivered %q, want the latest step's c", got)
	}
}

// TestWithRecordedSinks_ResumeKeepsItsStepSiblings: a step recorded [c d] and paused on c; resuming c
// alone still delivers d beside it.
func TestWithRecordedSinks_ResumeKeepsItsStepSiblings(t *testing.T) {
	sessions := session.InMemoryService()
	svc := artifact.InMemoryService()
	o := &Orchestrator{sessions: sessions, artifacts: svc, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := context.Background()
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "c", "", dag.DagPlanRecord{
		PlanID: "p", Sinks: []string{"c", "d"},
		Assignments: []dag.Assignment{{NodeID: "c", Task: "tc"}, {NodeID: "d", Task: "td", TaskID: "t4", Result: "D"}},
	}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "c"}, {ID: "d"}}}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, map[string]string{"c": "C"}, func(id string) bool { return id == "c" })
	if got, _ := o.sinkAnswer(plan, outputs, "c", recStopped); got != "## c\n\nC\n\n## d\n\nD" {
		t.Errorf("resume of c delivered %q, want its step's c and d", got)
	}
}

// TestWithRecordedSinks_StaleRecordGraphResume: the same stale record on the graph resume path, where
// what ran is what the run produced.
func TestWithRecordedSinks_StaleRecordGraphResume(t *testing.T) {
	sessions := session.InMemoryService()
	svc := artifact.InMemoryService()
	o := &Orchestrator{sessions: sessions, artifacts: svc, executor: dag.NewExecutor(sessions, nil, nil, nil, nil, nil)}
	ctx := context.Background()
	if _, _, err := dag.SaveDagPlanRecord(ctx, svc, artifactref.AppName, "u", "c", "", dag.DagPlanRecord{
		PlanID: "p", Sinks: []string{"a", "b"},
		Assignments: []dag.Assignment{{NodeID: "a", Task: "ta", Result: "A"}, {NodeID: "b", Task: "tb", Result: "B"}, {NodeID: "c", Task: "tc", DependsOn: []string{"b"}}},
	}); err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "a"}, {ID: "b"}, {ID: "c", DependsOn: []string{"b"}}}}
	resumed := map[string]string{"c": "C"}
	outputs, recStopped := o.withRecordedSinks(ctx, "u", "c", plan, resumed, func(id string) bool { _, ok := resumed[id]; return ok })
	if got, _ := o.sinkAnswer(plan, outputs, "c", recStopped); got != "C" {
		t.Errorf("graph resume delivered %q, want c alone", got)
	}
}
