package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// AssignmentFreshnessFunc judges a reused node's prior context (e.g. base_sha vs the branch tip);
// fresh=false runs the node id on a new session instead of resuming. nil means always fresh.
type AssignmentFreshnessFunc func(ctx agent.Context, planID, agentName, contextID string, a dag.Assignment) (fresh bool, reason string)

// ProvisionFunc provisions a plan's setup; nil skips it (tests, plan-only runs).
type ProvisionFunc func(ctx context.Context, userID, chatID string, plan *dag.Plan) error

// RunStepFunc runs exactly run's nodes, seeding the rest from seeded. started tells a node that never
// dispatched (a dependency paused first) from one that ran and produced nothing.
type RunStepFunc func(ctx context.Context, plan dag.Plan, seeded map[string]string, run map[string]bool) (outputs map[string]string, needsInput map[string]bool, started map[string]bool, err error)

// FinalizeAnswerFunc turns node outputs into the user-facing answer; it lives in the orchestrator,
// which owns the model for the optional format pass.
type FinalizeAnswerFunc func(ctx context.Context, plan dag.Plan, outputs map[string]string) string

type executeArgs struct {
	PlanID string `json:"plan_id"` // the plan_id create_plan/edit_plan returned
}

// assignmentResult lets the model plan next without a separate list_nodes round-trip.
type assignmentResult struct {
	NodeID    string   `json:"node_id"`
	Status    string   `json:"status"` // done|failed|paused|cancelled (user stopped)|queued (a dependency paused first)
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	TaskID    string   `json:"task_id"`
}

// executeResult.Status: running (no delivery yet), delivered, paused (awaits the user's answer),
// stopped (every delivering node was stopped) or needs_user (the judge keeps rejecting the same plan).
type executeResult struct {
	Status  string             `json:"status"`
	Results []assignmentResult `json:"results,omitempty"`
	// Message replaces Results when nothing new ran, naming the plan's status so the model doesn't
	// re-issue the identical call.
	Message string `json:"message,omitempty"`
}

// ExecPlanKey: session-state key for the selected plan (full JSON), so a retry finds it in persisted session.
const ExecPlanKey = "orch.exec.plan"

// summaryPreviewLen: enough output to judge what happened; the full text already streamed over SSE.
const summaryPreviewLen = 400

// NewExecuteTool runs one step: every assignment without a task_id, after planner.Build and its judge.
// The turn ends only when a step delivers or pauses; a rejection is a tool error plus a judge_round.
func NewExecuteTool(planner *dag.Planner, c *recordstore.Client, cache *PlanCache, provision ProvisionFunc, runStep RunStepFunc, finalize FinalizeAnswerFunc, history []dag.HistoryTurn, message string, attachments []*genai.Part, githubSetup *dag.Setup, allowedKinds []string, workerAsk string, contextItems []dag.ContextItem, planOnly bool, nodeID string, freshnessCheck AssignmentFreshnessFunc) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to run every assignment in the plan create_plan/edit_plan built that hasn't run yet. " +
				"Pass the plan_id it returned. Reads the plan's latest revision, runs the plan judge against it and " +
				"this conversation, and - if accepted - runs the new assignments now, returning each one's status, " +
				"a preview of its result, its artifacts, and task_id. A rejection comes back as an error naming what " +
				"to fix, so edit_plan and call execute again. If the plan does not yet declare `delivery`, this is a " +
				"partial step - read the results, then either edit_plan to add the next assignment(s) (a dependency " +
				"on a node that already ran hands it that node's result) or call execute again once you have. A " +
				"`failed` assignment cannot be retried within this plan - edit_plan refuses to remove or reassign it " +
				"once it has run; call create_plan for a fresh attempt at that work instead of trying edit_plan again. " +
				"Once the plan declares `delivery`, this call is terminal: the answer is shown to the user directly, " +
				"and you must output nothing further - no acknowledgement, no restatement, and never say a specialist " +
				"will respond (the work is already done).",
		},
		func(tc agent.Context, a executeArgs) (executeResult, error) {
			rec, shortCircuit, err := planForExecute(tc, c, a)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			if shortCircuit {
				return executeResult{
					Status:  runningStatus(rec.Status),
					Message: fmt.Sprintf("nothing new to run - plan %s status %q; every assignment has already run", rec.PlanID, rec.Status),
				}, nil
			}

			rawNodes, err := resolveRawNodes(tc, rec, c, freshnessCheck)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}

			// The trigger's repo/base_ref/branch win over the record's copy.
			setup := rec.Setup
			if githubSetup != nil {
				s := *githubSetup
				setup = &s
			}

			// ChatID-only Coords: plan judge's chat call files under this chat.
			planCtx := ledger.WithCoords(tc, ledger.Coords{ChatID: tc.SessionID()})
			shape := PlanShape(rawNodes, rec.Delivery)
			if key := waivedPlanShape(tc); key != "" && key == ShapeKey(shape) {
				planCtx = dag.WithPlanJudgeWaived(planCtx)
			}
			plan, err := planner.Build(planCtx, rawNodes, setup, rec.Delivery, history, message, attachments, allowedKinds)
			if err != nil {
				if recordPlanRejection(tc, c, nodeID, cache, err, shape) {
					return planLoopResult(tc, rec.PlanID), nil
				}
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			// Keep the id the model knows, and the Delivery assemble() resolved, so delivery decisions agree.
			plan.ID = rec.PlanID
			rec.Delivery = plan.Delivery
			plan.WorkerBackground = workerAsk
			plan.ContextItems = contextItems
			plan.PlanOnly = planOnly
			existingHead := ""
			if githubSetup != nil && githubSetup.CheckoutExistingHead {
				existingHead = githubSetup.WorkBranch
			}
			if err := dag.OverrideExistingPRHead(plan, existingHead); err != nil {
				inference.RecordPlanRejection(tc.SessionID(), err.Error())
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			// An accepted plan means an earlier rejection no longer explains how the run ends.
			inference.ClearPlanRejection(tc.SessionID())

			if err := provisionAndPersist(tc, plan, provision, cache); err != nil {
				return executeResult{}, err
			}

			results, stepPaused, stepFailed, err := executeStep(tc, c, &rec, plan, runStep)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}

			return finishExecStep(tc, c, cache, finalize, nodeID, &rec, plan, results, stepPaused, stepFailed)
		},
	)
}

// planForExecute loads the chat's dag_plan, validates the plan_id, and reports
// whether this call has nothing new to run - setting SkipSummarization if the plan is done.
func planForExecute(tc agent.Context, c *recordstore.Client, a executeArgs) (dag.DagPlanRecord, bool, error) {
	rec, ok, err := loadDagPlan(tc, c)
	if err != nil {
		return dag.DagPlanRecord{}, false, err
	}
	if !ok {
		return dag.DagPlanRecord{}, false, fmt.Errorf("no plan exists yet - call create_plan first")
	}
	if a.PlanID != "" && a.PlanID != rec.PlanID {
		return dag.DagPlanRecord{}, false, fmt.Errorf("unknown plan_id %q - the current plan is %q", a.PlanID, rec.PlanID)
	}
	if run, _ := partitionAssignments(rec.Assignments); len(run) == 0 {
		// Nothing new: don't re-run an unchanged plan; only "done" ends the turn (the repeat guard backstops).
		if rec.Status == "done" {
			tc.Actions().SkipSummarization = true
		}
		return rec, true, nil
	}
	return rec, false, nil
}

// resolveRawNodes maps the plan's assignments to raw DAG nodes, seeding a
// resumable node's own session unless freshnessCheck judges its context stale.
func resolveRawNodes(tc agent.Context, rec dag.DagPlanRecord, c *recordstore.Client, freshnessCheck AssignmentFreshnessFunc) ([]dag.RawNode, error) {
	nodes, err := listDagNodeRecords(tc, c)
	if err != nil {
		return nil, err
	}
	nodeAgent := make(map[string]string, len(nodes))
	resumedFrom := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeAgent[n.NodeID] = n.Agent
		if resumable, _ := n.Resumable(); resumable && n.ContextID != "" {
			resumedFrom[n.NodeID] = n.ContextID
		}
	}
	if freshnessCheck != nil {
		for _, a := range rec.Assignments {
			if resumedFrom[a.NodeID] == "" {
				continue
			}
			if fresh, reason := freshnessCheck(tc, rec.PlanID, nodeAgent[a.NodeID], resumedFrom[a.NodeID], a); !fresh {
				slog.Info("reused node's context is stale; running a fresh session",
					"component", "execute", "node", a.NodeID, "reason", reason)
				delete(resumedFrom, a.NodeID)
			}
		}
	}
	return dag.AssignmentsToRawNodes(rec.Assignments, nodeAgent, resumedFrom)
}

// recordPlanRejection: the judge_round save is best-effort, only logged. tripped: the loop guard tripped.
func recordPlanRejection(tc agent.Context, c *recordstore.Client, nodeID string, cache *PlanCache, err error, shape string) (tripped bool) {
	reason := err.Error()
	var rejected *dag.PlanRejectedError
	if errors.As(err, &rejected) {
		reason = rejected.Reason
		tripped = cache.RecordRejection(reason, shape)
		if _, _, jerr := vetting.SavePlanRejectionJudgeRound(tc, c, nodeID, tc.InvocationID(), reason); jerr != nil {
			slog.Warn("plan rejection judge_round save failed", "component", "execute", "err", jerr)
		}
	}
	inference.RecordPlanRejection(tc.SessionID(), reason)
	return tripped
}

// planLoopResult ends the turn once re-planning loops; the orchestrator then asks the user whether
// to run the plan as is, whatever model is planning.
func planLoopResult(tc agent.Context, planID string) executeResult {
	slog.Info("plan loop guard tripped: asking the user instead of re-planning", "component", "execute", "plan", planID, "chat", tc.SessionID())
	tc.Actions().SkipSummarization = true
	return executeResult{Status: "needs_user", Message: fmt.Sprintf("the plan judge keeps rejecting essentially the same plan %s - stop re-planning. "+
		"The user is asked whether to run it as is or rephrase; output nothing further this turn.", planID)}
}

// provisionAndPersist fails on a failed persist: ExecPlanKey is the cross-restart resume record.
func provisionAndPersist(tc agent.Context, plan *dag.Plan, provision ProvisionFunc, cache *PlanCache) error {
	if provision != nil {
		if err := provision(tc, tc.UserID(), tc.SessionID(), plan); err != nil {
			return fmt.Errorf("execute: %w", err)
		}
	}
	planJSON, err := json.Marshal(*plan)
	if err != nil {
		return fmt.Errorf("execute: marshal plan: %w", err)
	}
	if err := tc.State().Set(ExecPlanKey, string(planJSON)); err != nil {
		return fmt.Errorf("execute: persist plan for resume: %w", err)
	}
	cache.SetSelected(plan.ID)
	if yieldFn, ok := stream.YieldFromContext(tc); ok {
		yieldFn(DagPlanEvent(tc, *plan))
	}
	return nil
}

// executeStep dispatches this step's never-run assignments via runStep and turns
// each outcome into the model-facing results - "queued" means requested, never dispatched.
func executeStep(tc agent.Context, c *recordstore.Client, rec *dag.DagPlanRecord, plan *dag.Plan, runStep RunStepFunc) (results []assignmentResult, stepPaused, stepFailed bool, err error) {
	run, seeded := partitionAssignments(rec.Assignments)
	stopped := NodeStoppedFromContext(tc)
	for nid := range run {
		if stopped(nid) {
			return nil, false, false, errStoppedNode(nid)
		}
	}
	var outputs map[string]string
	var needsInput map[string]bool
	var started map[string]bool
	if runStep != nil {
		outputs, needsInput, started, err = runStep(dag.WithUnreviewedSeeds(tc, UnreviewedSeeds(rec.Assignments)), *plan, seeded, run)
		if err != nil {
			return nil, false, false, fmt.Errorf("run: %w", err)
		}
	} else {
		// No run function wired (a test double) - treat the whole run set as started.
		started = run
	}
	stepPaused = len(needsInput) > 0

	results = make([]assignmentResult, 0, len(run))
	for i := range rec.Assignments {
		nid := rec.Assignments[i].NodeID
		if !run[nid] {
			continue
		}
		// Never dispatched (a dependency paused first): leave it untouched so it stays reassignable.
		if !started[nid] {
			results = append(results, assignmentResult{NodeID: nid, Status: "queued", TaskID: rec.Assignments[i].TaskID})
			continue
		}
		out := outputs[nid]
		status := ApplyAssignmentOutcome(&rec.Assignments[i], out, needsInput[nid], stopped(nid))
		if status == "failed" {
			stepFailed = true
		}
		summary := previewText(out)
		if status == "cancelled" {
			summary = stoppedSummary
		}
		arts, _ := nodeArtifactIDs(tc, c, nid)
		results = append(results, assignmentResult{
			NodeID: nid, Status: status, Summary: summary, Artifacts: arts, TaskID: rec.Assignments[i].TaskID,
		})
	}
	return results, stepPaused, stepFailed, nil
}

// finishExecStep ends the turn on delivery or pause; a failed step leaves it open for the model to react.
func finishExecStep(tc agent.Context, c *recordstore.Client, cache *PlanCache, finalize FinalizeAnswerFunc, nodeID string, rec *dag.DagPlanRecord, plan *dag.Plan, results []assignmentResult, stepPaused, stepFailed bool) (executeResult, error) {
	// A failed or paused step must not mark the plan done and finalize on garbage.
	sinks := stepSinks(*plan, results, false)
	terminalStopped := rec.Delivery != nil && allStopped(results, sinks)
	delivering := rec.Delivery != nil && !stepPaused && !stepFailed && !terminalStopped && len(sinks) > 0
	// Queued sinks count: a paused step's resume delivers them once they run.
	rec.Sinks = stepSinks(*plan, results, true)
	rec.Status = "running"
	if delivering {
		rec.Status = "done"
	}
	runLineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
	_, step, serr := c.SaveStructured(tc, "dag_plan", rec, "", runLineage)
	if serr != nil {
		slog.Warn("execute: dag_plan step update failed", "component", "execute", "err", serr)
	}
	emitPlanEvent(tc, plan, step)

	if delivering && finalize != nil {
		cache.SetDelivered(finalize(tc, *plan, ownSinkResults(rec.Assignments, sinks)))
	}
	if delivering || stepPaused || terminalStopped {
		// ponytail: synthesizer node IS the loop-back. Add a caller-side knob when orchestrator needs to reshape.
		tc.Actions().SkipSummarization = true
	}

	slog.Info("execute: plan step ran", "component", "execute", "plan", plan.ID, "ran", len(results), "delivering", delivering, "paused", stepPaused, "failed", stepFailed)
	return executeResult{Status: stepStatus(delivering, stepPaused, terminalStopped), Results: results}, nil
}

func stepStatus(delivering, paused, stopped bool) string {
	switch {
	case delivering:
		return "delivered"
	case paused:
		return "paused"
	case stopped:
		return "stopped"
	}
	return "running"
}

// partitionAssignments: never-run assignments (no task_id) and the already-run results that seed them.
func partitionAssignments(assignments []dag.Assignment) (run map[string]bool, seeded map[string]string) {
	run = make(map[string]bool, len(assignments))
	seeded = make(map[string]string, len(assignments))
	for _, a := range assignments {
		if a.TaskID == "" {
			run[a.NodeID] = true
		} else {
			seeded[a.NodeID] = a.Result
		}
	}
	return run, seeded
}

// ApplyAssignmentOutcome is the one place a step dispatch and a HITL resume decide success, so they
// can't drift apart.
func ApplyAssignmentOutcome(a *dag.Assignment, output string, paused, stopped bool) (status string) {
	switch {
	case stopped:
		// Ran and was stopped by the user: never re-dispatched, its draft kept for dependents.
		a.TaskID = uuid.NewString()
		a.Result, a.Stopped = output, true
		return "cancelled"
	case strings.TrimSpace(output) != "":
		a.TaskID = uuid.NewString()
		a.Result, a.Stopped = output, false
		return "done"
	case paused:
		// Parked on a HITL question: no task_id, so it's still "to run" (see dag.PlanStepSessionID).
		return "paused"
	default:
		a.TaskID = uuid.NewString()
		return "failed"
	}
}

// runningStatus maps a dag_plan status to executeResult's vocabulary for the "nothing new" path.
func runningStatus(recStatus string) string {
	if recStatus == "done" {
		return "delivered"
	}
	return "running"
}

func previewText(s string) string {
	s = stream.StripThinking(s)
	if len(s) <= summaryPreviewLen {
		return s
	}
	return s[:summaryPreviewLen] + "…"
}

// UnreviewedSeeds flags the stopped assignments whose drafts seed later nodes, so those
// nodes are told the input never passed review (as retry and boot-resume seeds are).
func UnreviewedSeeds(assignments []dag.Assignment) map[string]bool {
	out := map[string]bool{}
	for _, a := range assignments {
		if a.Stopped && a.Result != "" {
			out[a.NodeID] = true
		}
	}
	return out
}

// DeliverableResults: a stopped assignment's draft reads as empty, so it never becomes the delivered answer.
func DeliverableResults(assignments []dag.Assignment) (final map[string]string, stopped map[string]bool) {
	final = make(map[string]string, len(assignments))
	stopped = map[string]bool{}
	for _, a := range assignments {
		if a.Stopped {
			final[a.NodeID], stopped[a.NodeID] = "", true
			continue
		}
		final[a.NodeID] = a.Result
	}
	return final, stopped
}

// stoppedSummary tells the orchestrator model what a cancelled assignment means.
const stoppedSummary = "stopped by the user before it finished - do not re-run or re-plan this work, " +
	"and do not relay or restate its unreviewed draft as your answer"

// allStopped reports this step stopped every one of sinks, so it has no answer to deliver.
func allStopped(results []assignmentResult, sinks []string) bool {
	cancelled := map[string]bool{}
	for _, r := range results {
		cancelled[r.NodeID] = r.Status == "cancelled"
	}
	for _, id := range sinks {
		if !cancelled[id] {
			return false
		}
	}
	return len(sinks) > 0
}

// ownSinkResults: only this step's sinks, or finalize would also deliver an earlier turn's sinks.
func ownSinkResults(assignments []dag.Assignment, sinks []string) map[string]string {
	final, _ := DeliverableResults(assignments)
	own := make(map[string]string, len(sinks))
	for _, id := range sinks {
		own[id] = final[id]
	}
	return own
}

// stepSinks are the nodes a delivering step answers with: every node this step ran that no other
// node it ran depends on - in an extended plan, not an earlier turn's (re-wired) sink.
func stepSinks(plan dag.Plan, results []assignmentResult, withQueued bool) []string {
	ran := make(map[string]bool, len(results))
	for _, r := range results {
		if withQueued || r.Status != "queued" {
			ran[r.NodeID] = true
		}
	}
	return dag.SinksAmong(plan.Nodes, func(id string) bool { return ran[id] })
}

// DeliveredAnswer is what the plan's sinks in outputs deliver: one sink's output as is, or each as its own
// labelled section in plan order (sectioned). A stopped sink's draft never ships.
func DeliveredAnswer(plan dag.Plan, outputs map[string]string, stopped func(string) bool) (answer string, sectioned bool) {
	terminals := dag.TerminalIDs(plan.Nodes)
	sinks := presentLeaves(plan, outputs, terminals)
	if len(sinks) == 0 {
		// A step's own sinks can sit upstream of an earlier turn's re-wired synthesizer.
		if slices.ContainsFunc(terminals, stopped) {
			return "", false
		}
		sinks = presentLeaves(plan, outputs, nil)
	}
	switch len(sinks) {
	case 0:
		return "", false
	case 1:
		if stopped(sinks[0]) {
			return "", false
		}
		return stream.StripThinking(outputs[sinks[0]]), false
	}
	return sinkSections(sinks, outputs, stopped), true
}

// presentLeaves: nodes of among (all when nil) with an output and no successor that has one.
func presentLeaves(plan dag.Plan, outputs map[string]string, among []string) []string {
	succeeded := map[string]bool{}
	for _, n := range plan.Nodes {
		if _, ok := outputs[n.ID]; ok {
			for _, dep := range n.DependsOn {
				succeeded[dep] = true
			}
		}
	}
	var leaves []string
	for _, n := range plan.Nodes {
		if _, ok := outputs[n.ID]; ok && !succeeded[n.ID] && (among == nil || slices.Contains(among, n.ID)) {
			leaves = append(leaves, n.ID)
		}
	}
	return leaves
}

// sinkSections labels each sink by node id - unique, unlike the agent names the UI titles nodes with.
func sinkSections(sinks []string, outputs map[string]string, stopped func(string) bool) string {
	parts := make([]stream.SinkAnswer, 0, len(sinks))
	anyLive := false
	for _, id := range sinks {
		st := stopped(id)
		anyLive = anyLive || !st
		parts = append(parts, stream.SinkAnswer{Label: id, Text: stream.StripThinking(outputs[id]), Stopped: st})
	}
	if !anyLive {
		return ""
	}
	return stream.JoinSinkAnswers(parts)
}
