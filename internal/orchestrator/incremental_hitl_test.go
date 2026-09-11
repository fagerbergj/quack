// incremental_hitl_test.go: BLOCKING 1 (#slice3 review) - a node paused
// mid-incremental-step (dag.Executor.RunPlanStep, its own dedicated
// dag.PlanStepSessionID session) must still be answerable through the
// normal StartNode entrypoint (Chat.tsx's Answer button), not just
// cancellable. Proves the real pause -> StartNode -> ResumePlanStep path,
// end to end through the orchestrator.
package orchestrator

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

type askHITLArgs struct {
	Question string `json:"question"`
}
type askHITLResult struct {
	Status string `json:"status"`
}

// newHITLAskTool mirrors dag's own newAskTool (internal/dag/hitl_test.go) -
// the gate watches for this call by name and pauses the node; built here
// too since a cross-package test helper import isn't worth it for one tool.
func newHITLAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askHITLArgs, askHITLResult](
		functiontool.Config{Name: vetting.AskToolName, Description: "Ask the user a question."},
		func(tc adkagent.Context, _ askHITLArgs) (askHITLResult, error) {
			tc.Actions().SkipSummarization = true
			return askHITLResult{Status: "forwarded to the user"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// hitlOrchStub plays the orchestrator (create_plan(asker) -> auto-execute,
// via the embedded orchStub's own routing) AND the asker worker itself: asks
// a question, then answers once its prompt shows the user's reply.
type hitlOrchStub struct {
	orchStub
	sawAnswer string
}

func (s *hitlOrchStub) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	if stubHasTool(req, "submit_verdict") || stubHasTool(req, "create_plan") {
		return s.orchStub.GenerateContent(ctx, req, isStream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		txt := stubUserText(req)
		if strings.Contains(txt, "they answered") {
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.sawAnswer = line
			}
			yield(stubText("Final answer using the user's direction."), nil)
			return
		}
		yield(stubCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
	}
}

// askerPlanCall authors a single-assignment, delivering plan for the asker -
// delivery is declared so a successful resume finalizes, proving the whole
// round trip, not just that the node itself finishes.
func askerPlanCall() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{
		"assignments": []any{map[string]any{"agent": "asker", "task": "ask the direction"}},
		"delivery":    map[string]any{"kind": "comment"},
	})
}

func newHITLTestOrch(t *testing.T, stub model.LLM, askTool tool.Tool) *Orchestrator {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "asker", Model: stub, Description: "asker", Instruction: "ROLE:asker Answer.",
		Tools: []tool.Tool{askTool},
	})
	if err != nil {
		t.Fatalf("asker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"asker": worker},
		map[string]model.LLM{"asker": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "asker", Description: "asks the user things"}}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
	// A real dag_plan record store, shared across Run() and StartNode() calls -
	// without it each falls back to its OWN ephemeral in-memory service and
	// StartNode can never see what Run() wrote (needed for this test's resume).
	o.SetArtifacts(artifact.InMemoryService())
	return o
}

// TestOrchestrator_NodePausedMidStep_StartNodeResumesAndFinishes is BLOCKING
// 1's proof: step 1 dispatches the asker through the REAL RunPlanStep, it
// pauses on a question (its own dedicated plan-step session, not the chat
// one), and StartNode's answer resumes and finishes it - the plan's declared
// delivery then finalizes, proving the dag_plan record was updated too.
func TestOrchestrator_NodePausedMidStep_StartNodeResumesAndFinishes(t *testing.T) {
	stub := &hitlOrchStub{orchStub: orchStub{replies: []*model.LLMResponse{askerPlanCall()}}}
	o := newHITLTestOrch(t, stub, newHITLAskTool(t))

	evs := runTurn(t, o, "ask the direction")
	if hasEvent(evs, stream.EventError) {
		t.Fatalf("turn 1 surfaced an error; events=%v", evs)
	}
	if got := countEvent(evs, stream.EventNodeNeedsInput); got != 1 {
		t.Fatalf("turn 1: node_needs_input events = %d, want 1; events=%v", got, evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != "" {
		t.Fatalf("turn 1: answer = %q, want empty - the node hasn't answered yet", answer)
	}

	// The chat session itself has no pending question (StartNode's OLD path
	// would find nothing there) - it lives on the plan-step session.
	if _, ok := latestPendingNodeInterrupt(o.PriorEvents(context.Background(), "u", "chat")); ok {
		t.Fatal("chat session reports a pending interrupt - the node paused inside RunPlanStep, on its own session")
	}
	pend, ok := o.pendingStepInterrupt(context.Background(), "u", "chat")
	if !ok || pend.nodeID != "asker-1" {
		t.Fatalf("pendingStepInterrupt = (%+v, %v), want asker-1's paused question", pend, ok)
	}

	var resumeEvs []stream.SSEEvent
	collect := func(ev stream.SSEEvent, err error) bool {
		if err == nil {
			resumeEvs = append(resumeEvs, ev)
		}
		return true
	}
	o.StartNode(context.Background(), "u", "chat", "asker-1", "north", collect)

	if hasEvent(resumeEvs, stream.EventError) {
		t.Fatalf("StartNode resume surfaced an error; events=%v", resumeEvs)
	}
	if stub.sawAnswer != "north" {
		t.Errorf("asker never received the answer (saw %q)", stub.sawAnswer)
	}
	if got := countEvent(resumeEvs, stream.EventNodeDone); got != 1 {
		t.Errorf("resume: node_done events = %d, want 1; events=%v", got, resumeEvs)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "Final answer using the user's direction.") {
		t.Errorf("answer = %q, want the asker's post-resume output - delivery must have finalized", answer)
	}
}
