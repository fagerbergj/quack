// incremental_hitl_test.go: BLOCKING 1 (#slice3 review) - a node paused
// mid-incremental-step (dag.Executor.RunPlanStep, its own dedicated
// dag.PlanStepSessionID session) must still be answerable through the
// normal StartNode entrypoint (Chat.tsx's Answer button), not just
// cancellable. Proves the real pause -> StartNode -> ResumePlanStep path,
// end to end through the orchestrator.
package orchestrator

import (
	"context"
	"encoding/json"
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
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
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

// failingHITLOrchStub is hitlOrchStub except every worker round after the
// initial question comes back empty - the gate's own continuation loop and
// writer-recovery fallback (internal/vetting/node.go) both run out on empty
// text too, so the node ends in ErrNodeEmpty, same as any other failed node.
// Keyed on "asked" rather than prompt content: the continuation and
// writer-recovery prompts don't carry the same "they answered" marker the
// post-resume prompt does, so a content match only reproduces a second
// pause, not the failure this test needs.
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

// TestOrchestrator_ResumedNodeFails_ReportsFailedAndDoesNotFinalize pins the
// shared-guard fix (#slice3 review): a resumed node that finishes with empty
// output is exactly as failed as a freshly-run one - it must not be
// silently marked done, and a plan whose delivery depended on it must not
// finalize on the failure.
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
	o.StartNode(context.Background(), "u", "chat", "asker-1", "north", collect)

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

// stubSystemInstruction reads req's system instruction text - unlike the
// user content (stubUserText), which can carry shared plan/task context a
// sibling node's prompt also echoes, each worker's OWN Instruction
// (llmagent.Config) is unique to its agent, so it's the reliable way to
// tell which of two DIFFERENT agents' workers this call is for.
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

// bcHitlStub plays the orchestrator (a 2-node create_plan: asker -> closer,
// closer depends_on asker) AND both workers - asker asks then answers,
// closer (the terminal, delivering node) just answers once it sees the
// dependency handoff. Routed on the system instruction (ROLE:asker vs
// ROLE:closer, each llmagent's own), not task text - a node's prompt can
// echo shared plan context another node's task text also appears in.
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

// askerOnlyPartialPlanCall authors a single-node, no-delivery-yet plan (a
// legitimate partial step, #slice3) - closer is added directly to the
// record afterward (see spliceClosterIntoRecord), keeping this test's
// setup isolated from a separate, pre-existing gap: a fresh execute() step
// that bundles a not-yet-ready dependent (closer, depending on the SAME
// step's still-pausing asker) into its own run set marks that dependent
// "failed" rather than leaving it unrun - a real behavior, but a different
// one than the resume-path gap this test targets.
func askerOnlyPartialPlanCall() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{
		"assignments": []any{map[string]any{"agent": "asker", "task": "ask the direction"}},
	})
}

// spliceCloserIntoRecord adds closer-1 (depends_on asker-1, TaskID=="") to
// the dag_plan record AND the stashed dag.Plan session state execute.go
// writes on judge acceptance (tools.ExecPlanKey) - RunPlanStep dispatches
// off the STASHED PLAN's own Node list, not the record, so both must agree
// for a resume to actually be able to run closer-1. Mirrors what a real
// edit_plan + execute() round trip would have produced, without going
// through a full extra turn (and the separate gap noted above).
func spliceCloserIntoRecord(t *testing.T, o *Orchestrator) {
	t.Helper()
	ctx := context.Background()
	rec, _, ok, err := dag.LoadDagPlanRecord(ctx, o.artifacts, artifactref.AppName, "u", "chat")
	if err != nil || !ok {
		t.Fatalf("LoadDagPlanRecord: ok=%v err=%v", ok, err)
	}
	rec.Assignments = append(rec.Assignments, dag.Assignment{NodeID: "closer-1", Task: "close it out", DependsOn: []string{"asker-1"}})
	rec.Delivery = &dag.Delivery{Kind: "comment"}
	rc := recordstore.New(o.artifacts, artifactref.AppName, "u", "chat")
	if _, _, err := rc.SaveStructured(ctx, "dag_node", dag.DagNodeRecord{NodeID: "closer-1", Agent: "closer"}, "closer-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed closer-1 dag_node: %v", err)
	}
	if _, _, err := dag.SaveDagPlanRecord(ctx, o.artifacts, artifactref.AppName, "u", "chat", "", rec); err != nil {
		t.Fatalf("save dag_plan with closer-1: %v", err)
	}

	// session.Service.Get returns a COPY of the session (inmemory.go's own
	// Get clones state/events) - Session.State().Set on it never persists.
	// The real, durable write path is AppendEvent with a StateDelta (what a
	// live tool call's tc.State().Set ultimately turns into once ADK
	// commits that call's own event) - mirrored here directly.
	resp, err := o.sessions.Get(ctx, &session.GetRequest{AppName: AppName, UserID: "u", SessionID: "chat"})
	if err != nil || resp == nil {
		t.Fatalf("session.Get: resp=%v err=%v", resp, err)
	}
	plan, ok := o.stashedPlan(ctx, "u", "chat")
	if !ok {
		t.Fatal("stashedPlan: no plan stashed after turn 1")
	}
	plan.Nodes = append(plan.Nodes, dag.Node{ID: "closer-1", AgentName: "closer", Task: "close it out", DependsOn: []string{"asker-1"}})
	plan.Delivery = rec.Delivery
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal stashed plan: %v", err)
	}
	ev := session.NewEvent(ctx, "")
	ev.Author = "test"
	ev.Actions.StateDelta = map[string]any{tools.ExecPlanKey: string(planJSON)}
	if err := o.sessions.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("stash grown plan: %v", err)
	}
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
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{
		{Name: "asker", Description: "asks the user things"},
		{Name: "closer", Description: "closes out the work"},
	}, nil, nil)
	o := New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())
	return o
}

// TestOrchestrator_ResumeDrivesUnblockedDependent_BThenCDelivers is the
// reviewer's [IMPORTANT] finding (#slice3 review, orchestrator.go ~1111): a
// resume dispatch that only re-runs the paused node (B) never drives its
// now-unblocked dependent (C, the terminal/delivering node) - the turn used
// to finalize on C's still-empty output (or, per this fix, drive C too and
// finalize on ITS output). B -> C, C depends_on B, C is terminal.
func TestOrchestrator_ResumeDrivesUnblockedDependent_BThenCDelivers(t *testing.T) {
	stub := &bcHitlStub{orchStub: orchStub{replies: []*model.LLMResponse{askerOnlyPartialPlanCall()}}}
	o := newBCHitlTestOrch(t, stub, newHITLAskTool(t))

	evs := runTurn(t, o, "ask the direction")
	if hasEvent(evs, stream.EventError) {
		t.Fatalf("turn 1 surfaced an error; events=%v", evs)
	}
	if got := countEvent(evs, stream.EventNodeNeedsInput); got != 1 {
		t.Fatalf("turn 1: node_needs_input events = %d, want 1; events=%v", got, evs)
	}

	// closer-1 (C, terminal/delivering, depends_on asker-1) joins the plan
	// AFTER asker (B) has already paused - what a real edit_plan + execute()
	// round trip would produce; see spliceCloserIntoRecord's own doc for why
	// this test builds it directly instead of through a second live turn.
	spliceCloserIntoRecord(t, o)
	if plan, ok := o.stashedPlan(context.Background(), "u", "chat"); !ok || len(plan.Nodes) != 2 {
		t.Fatalf("stashedPlan after splice = (%+v, %v), want 2 nodes (asker-1, closer-1)", plan, ok)
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
	// Both B (asker) and C (closer) must finish within this SAME resume turn -
	// the whole point of driving now-unblocked dependents after the paused
	// node completes.
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
