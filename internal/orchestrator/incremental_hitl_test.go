// A node paused mid-incremental-step must be answerable through StartNode, end to end.
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

	"github.com/fagerbergj/quack/internal/artifactref"
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

// newHITLAskTool mirrors dag's newAskTool: the gate pauses the node on this call by name.
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

// hitlOrchStub plays the orchestrator and the asker worker: it asks, then answers once its
// prompt shows the user's reply.
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

// askerPlanCall declares delivery so a successful resume finalizes, proving the round trip.
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
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "asker", Description: "asks the user things"}}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
	// Shared record store: otherwise StartNode can't see what Run() wrote.
	o.SetArtifacts(artifact.InMemoryService())
	return o
}

// The asker pauses in its own plan-step session; StartNode's answer resumes and finishes it,
// and the declared delivery finalizes from the updated record.
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
	o.StartNode(context.Background(), "u", "chat", "", "asker-1", "north", collect)

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

// failingHITLOrchStub returns empty text on every round after the question, so the node ends
// in ErrNodeEmpty. Keyed on "asked": recovery prompts lack the post-resume marker.
type failingHITLOrchStub struct {
	orchStub
	asked bool
}

func (s *failingHITLOrchStub) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	if stubHasTool(req, "submit_verdict") || stubHasTool(req, "create_plan") {
		return s.orchStub.GenerateContent(ctx, req, isStream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if !s.asked {
			s.asked = true
			yield(stubCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
			return
		}
		yield(stubText(""), nil) // every round from here on: the worker produces nothing
	}
}

// A resumed node with empty output fails like a fresh one: not marked done, and its
// dependent delivery must not finalize.
func TestOrchestrator_ResumedNodeFails_ReportsFailedAndDoesNotFinalize(t *testing.T) {
	stub := &failingHITLOrchStub{orchStub: orchStub{replies: []*model.LLMResponse{askerPlanCall()}}}
	o := newHITLTestOrch(t, stub, newHITLAskTool(t))

	evs := runTurn(t, o, "ask the direction")
	if hasEvent(evs, stream.EventError) {
		t.Fatalf("turn 1 surfaced an error; events=%v", evs)
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
	o.StartNode(context.Background(), "u", "chat", "", "asker-1", "north", collect)

	if hasEvent(resumeEvs, stream.EventError) {
		t.Fatalf("StartNode resume surfaced an error; events=%v", resumeEvs)
	}
	if got := countEvent(resumeEvs, stream.EventNodeFailed); got != 1 {
		t.Errorf("resume: node_failed events = %d, want 1; events=%v", got, resumeEvs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != "" {
		t.Errorf("answer = %q, want empty - a failed delivering node must not finalize", answer)
	}
}

// stubSystemInstruction reads the system instruction, which is unique per agent unlike the
// user content a sibling's prompt can echo.
func stubSystemInstruction(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// bcHitlStub plays the orchestrator (asker -> closer) and both workers, routed on each
// agent's system instruction rather than echoable task text.
type bcHitlStub struct {
	orchStub
	sawAnswer string
}

func (s *bcHitlStub) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	if stubHasTool(req, "submit_verdict") || stubHasTool(req, "create_plan") {
		return s.orchStub.GenerateContent(ctx, req, isStream)
	}
	txt := stubUserText(req)
	if strings.Contains(stubSystemInstruction(req), "ROLE:closer") {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(stubText("CLOSER-RESULT"), nil)
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if strings.Contains(txt, "they answered") {
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.sawAnswer = line
			}
			yield(stubText("ASKER-RESULT"), nil)
			return
		}
		yield(stubCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
	}
}

// askerCloserPlanCall hires asker and closer (depends_on asker) with delivery in one
// create_plan; asker pauses first, and closer must be left not-yet-run, not failed.
func askerCloserPlanCall() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{
		"assignments": []any{
			map[string]any{"agent": "asker", "task": "ask the direction"},
			map[string]any{"agent": "closer", "task": "close it out", "depends_on": []any{"0"}},
		},
		"delivery": map[string]any{"kind": "comment"},
	})
}

// newBCHitlTestOrch is newHITLTestOrch with a SECOND worker agent (closer,
// no HITL tool) sharing the same stub - the B -> C shape this test needs.
func newBCHitlTestOrch(t *testing.T, stub model.LLM, askTool tool.Tool) *Orchestrator {
	t.Helper()
	asker, err := llmagent.New(llmagent.Config{
		Name: "asker", Model: stub, Description: "asker", Instruction: "ROLE:asker Answer.",
		Tools: []tool.Tool{askTool},
	})
	if err != nil {
		t.Fatalf("asker agent: %v", err)
	}
	closer, err := llmagent.New(llmagent.Config{
		Name: "closer", Model: stub, Description: "closer", Instruction: "ROLE:closer Answer.",
	})
	if err != nil {
		t.Fatalf("closer agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"asker": asker, "closer": closer},
		map[string]model.LLM{"asker": stub, "closer": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{
		{Name: "asker", Description: "asks the user things"},
		{Name: "closer", Description: "closes out the work"},
	}, nil, nil)
	o := New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())
	return o
}

// One execute dispatches asker and closer; asker pauses, closer stays not-yet-run, then the
// resume completes asker, drives closer, and delivers its output.
func TestOrchestrator_ResumeDrivesUnblockedDependent_BThenCDelivers(t *testing.T) {
	stub := &bcHitlStub{orchStub: orchStub{replies: []*model.LLMResponse{askerCloserPlanCall()}}}
	o := newBCHitlTestOrch(t, stub, newHITLAskTool(t))

	evs := runTurn(t, o, "ask the direction")
	if hasEvent(evs, stream.EventError) {
		t.Fatalf("turn 1 surfaced an error; events=%v", evs)
	}
	if got := countEvent(evs, stream.EventNodeNeedsInput); got != 1 {
		t.Fatalf("turn 1: node_needs_input events = %d, want 1; events=%v", got, evs)
	}

	// closer-1 never started (asker paused first), so it must not be failed or minted a task_id.
	rec1, _, ok, err := dag.LoadDagPlanRecord(context.Background(), o.artifacts, artifactref.AppName, "u", "chat")
	if err != nil || !ok {
		t.Fatalf("LoadDagPlanRecord after turn 1: ok=%v err=%v", ok, err)
	}
	for _, a := range rec1.Assignments {
		if a.NodeID == "closer-1" && a.TaskID != "" {
			t.Fatalf("closer-1 task_id = %q after turn 1, want empty - it never started and must stay dispatchable", a.TaskID)
		}
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
	o.StartNode(context.Background(), "u", "chat", "", "asker-1", "north", collect)

	if hasEvent(resumeEvs, stream.EventError) {
		t.Fatalf("StartNode resume surfaced an error; events=%v", resumeEvs)
	}
	if stub.sawAnswer != "north" {
		t.Errorf("asker never received the answer (saw %q)", stub.sawAnswer)
	}
	// Both nodes must finish within this same resume turn.
	doneIDs := map[string]bool{}
	for _, ev := range resumeEvs {
		if d, ok := ev.Data.(stream.NodeDoneData); ok {
			doneIDs[d.NodeID] = true
		}
	}
	if !doneIDs["asker-1"] || !doneIDs["closer-1"] {
		t.Fatalf("resume: node_done ids = %v, want both asker-1 and closer-1", doneIDs)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "CLOSER-RESULT") {
		t.Errorf("answer = %q, want the terminal node's (closer) output - delivery must reflect what C produced, not B's", answer)
	}

	rec, _, ok, err := dag.LoadDagPlanRecord(context.Background(), o.artifacts, artifactref.AppName, "u", "chat")
	if err != nil || !ok {
		t.Fatalf("LoadDagPlanRecord: ok=%v err=%v", ok, err)
	}
	for _, a := range rec.Assignments {
		if a.TaskID == "" {
			t.Errorf("assignment %s never ran (empty task_id) after the resume turn", a.NodeID)
		}
	}
}
