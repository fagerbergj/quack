package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// pauseStepStub plans researcher -> closer; the researcher's first call blocks until its round is cut.
type pauseStepStub struct {
	orchStub
	researchCalls atomic.Int32
	started       chan struct{}
}

func (s *pauseStepStub) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	txt := stubUserText(req)
	if stubHasTool(req, "submit_verdict") || stubHasTool(req, "create_plan") || !strings.Contains(txt, "TASK-RESEARCH") {
		return s.orchStub.GenerateContent(ctx, req, isStream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if strings.Contains(txt, "TASK-CLOSE") {
			yield(stubText("CLOSER-RESULT"), nil)
			return
		}
		if s.researchCalls.Add(1) > 1 {
			yield(stubText("RESEARCH-RESULT"), nil)
			return
		}
		s.started <- struct{}{}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// executeResponse returns the execute tool's last response from the chat session.
func executeResponse(t *testing.T, o *Orchestrator) map[string]any {
	t.Helper()
	var resp map[string]any
	for _, ev := range o.PriorEvents(context.Background(), "u", "chat") {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "execute" {
				resp = p.FunctionResponse.Response
			}
		}
	}
	return resp
}

// A user pause mid-step parks the step: the dependent never starts, execute reports the node paused
// (so the orchestrator doesn't replan), and resuming it runs the node and then its dependent.
func TestOrchestrator_UserPausedNodeParksStepAndResumeFinishes(t *testing.T) {
	stub := &pauseStepStub{started: make(chan struct{}, 1), orchStub: orchStub{replies: []*model.LLMResponse{stubCall("create_plan", map[string]any{
		"assignments": []any{
			map[string]any{"agent": "w", "task": "TASK-RESEARCH zig"},
			map[string]any{"agent": "w", "task": "TASK-RESEARCH TASK-CLOSE compare", "depends_on": []any{"0"}},
		},
		"delivery": map[string]any{"kind": "comment"},
	})}}}
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "w", Description: "does the work"}}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())
	go func() {
		select {
		case <-stub.started:
			o.PauseNode("chat", "w-1", dag.PauseUser)
		case <-time.After(10 * time.Second):
		}
	}()

	evs := runTurn(t, o, "research zig then compare")
	if countEvent(evs, stream.EventNodePaused) != 1 || hasNodeStart(evs, "w-2") {
		t.Fatalf("want w-1 paused and w-2 never started; events=%v", evs)
	}
	if n := stub.invocations(); n != 1 {
		t.Errorf("orchestrator ran %d planning calls, want 1 - a paused node must not trigger a replan", n)
	}
	got, _ := json.Marshal(executeResponse(t, o))
	if !strings.Contains(string(got), `"status":"paused"`) || strings.Contains(string(got), `"failed"`) {
		t.Errorf("execute response = %s, want the step and w-1 paused, nothing failed", got)
	}

	resumeEvs := []stream.SSEEvent{}
	for ev := range o.ResumeNodes(context.Background(), "u", "chat", "", nil, []string{"w-1"}) {
		resumeEvs = append(resumeEvs, ev)
	}
	if !hasNodeStart(resumeEvs, "w-2") {
		t.Errorf("resume never ran the held dependent w-2; events=%v", resumeEvs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); !strings.Contains(answer, "CLOSER-RESULT") {
		t.Errorf("answer = %q, want the dependent's output once the resume ran it", answer)
	}
	rec, _, _, err := dag.LoadDagPlanRecord(context.Background(), o.artifacts, artifactref.AppName, "u", "chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rec.Assignments {
		if a.TaskID == "" || a.Result == "" {
			t.Errorf("assignment %s = %+v after the resume, want it settled with its result", a.NodeID, a)
		}
	}
}

func hasNodeStart(evs []stream.SSEEvent, nodeID string) bool {
	for _, ev := range evs {
		if d, ok := ev.Data.(stream.NodeStartData); ok && d.NodeID == nodeID {
			return true
		}
	}
	return false
}
