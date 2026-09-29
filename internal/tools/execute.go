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

// AssignmentFreshnessFunc optionally judges whether a reused node's prior
// context is still fresh before execute resumes it - e.g. the GitHub
// extension checking assignment.meta.github.base_sha against the branch's
// current tip. fresh=false runs the same node id on a brand-new session
// instead of resuming; nil skips the check (always fresh). planID/agentName/
// contextID aren't on dag.Assignment itself - passed through so the
// sdk.Assignment conversion at the wiring site (internal/serve) can
// populate the sdk struct fully.
type AssignmentFreshnessFunc func(ctx agent.Context, planID, agentName, contextID string, a dag.Assignment) (fresh bool, reason string)

// ProvisionFunc provisions a plan's setup (e.g. the executor's clone);
// nil skips provisioning (tests, plan-only runs).
type ProvisionFunc func(ctx context.Context, userID, chatID string, plan *dag.Plan) error

// RunStepFunc runs exactly the nodes named in run - fresh dispatches, never
// retries - seeding every other node's output from seeded, and returns what
// each ran node produced, which run-set node(s), if any, parked on a HITL
// question (nil/empty when none did), and which run-set node(s) actually
// reached running - a node whose dependency paused earlier in the SAME step
// is requested but never dispatched, and must be told apart from a node that
// ran and produced nothing (ApplyAssignmentOutcome must only be called for
// the latter).
// The orchestrator wires this to dag.Executor.RunPlanStep; a test can fake it directly.
type RunStepFunc func(ctx context.Context, plan dag.Plan, seeded map[string]string, run map[string]bool) (outputs map[string]string, needsInput map[string]bool, started map[string]bool, err error)

// FinalizeAnswerFunc turns a plan's accumulated node outputs into the user-
// facing answer (terminal node output, formatted if needed) - the
// orchestrator wires this to its own finalizeAnswer, which needs its model
// for the optional format pass, something execute.go (internal/tools) has
// no access to.
type FinalizeAnswerFunc func(ctx context.Context, plan dag.Plan, outputs map[string]string) string

type executeArgs struct {
	PlanID string `json:"plan_id"` // the plan_id create_plan/edit_plan returned
}

// assignmentResult is one just-run assignment's outcome, returned to the
// model so it can decide what - if anything - to plan next without a
// separate list_nodes round-trip.
type assignmentResult struct {
	NodeID    string   `json:"node_id"`
	Status    string   `json:"status"` // "done" | "failed" | "paused" | "cancelled" (the user stopped it) | "queued" (requested this step but never dispatched - a dependency paused first)
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	TaskID    string   `json:"task_id"`
}

// executeResult.Status: "running" (no delivery declared yet), "delivered", "paused" (a node awaits
// the user's answer - no execute/edit_plan until then), "stopped" (every delivering node was stopped)
// or "needs_user" (the plan judge keeps rejecting the same plan; the user decides).
type executeResult struct {
	Status  string             `json:"status"`
	Results []assignmentResult `json:"results,omitempty"`
	// Message: set instead of Results when this call had nothing new to run
	// (every assignment already has a task_id) - names the plan's current
	// status so the model doesn't re-issue the identical call expecting a
	// different answer.
	Message string `json:"message,omitempty"`
}

// ExecPlanKey: session-state key for the selected plan (full JSON), so a retry finds it in persisted session.
const ExecPlanKey = "orch.exec.plan"

// summaryPreviewLen caps how much of a just-run node's output execute()
// echoes back to the model - enough to judge what happened, not a re-read
// of the full output (already on the SSE stream as it ran).
const summaryPreviewLen = 400

// NewExecuteTool: reads the chat's current dag_plan record and its
// dag_node agents, runs planner.Build (unknown agent, cycle,
// review-deliverable, review-fanout, the plan judge, against this
// conversation's history/message), provisions Setup, and runs every
// assignment that hasn't run yet (dag.Assignment.TaskID == "") - one STEP of
// a plan that can keep growing via edit_plan. A rejection surfaces as a tool
// error - edit_plan and call execute again, and is also recorded as a
// judge_round anchored to nodeID (the authoring lineage id) so the artifact
// panel shows it. Every assignment naming a terminal (already-run) node id
// resumes that node's own session, unless freshnessCheck says it's stale.
// The tool ends the orchestrator's turn (SkipSummarization) only once a step
// declares delivery - otherwise it returns this step's results and the turn continues.
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

			// GitHub already told us repo/base_ref/branch - never trust the record's own copy.
			setup := rec.Setup
			if githubSetup != nil {
				s := *githubSetup
				setup = &s
			}

			// ChatID-only Coords: plan judge's chat call files under this chat.
			planCtx := ledger.WithCoords(tc, ledger.Coords{ChatID: tc.SessionID()})
			plan, err := planner.Build(planCtx, rawNodes, setup, rec.Delivery, history, message, attachments, allowedKinds)
			if err != nil {
				if recordPlanRejection(tc, c, nodeID, cache, err, PlanShape(rawNodes, rec.Delivery)) {
					return planLoopResult(tc, rec.PlanID), nil
				}
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			// assemble() mints its own fresh plan.ID - overwrite it with the
			// record's, the id the model actually knows and referenced.
			plan.ID = rec.PlanID
			// assemble() may resolve plan.Delivery from an explicit-but-kindless declaration
			// - sync it back so the delivering decision reads the SAME resolved value.
			rec.Delivery = plan.Delivery
			// Nodes get the ask-only background and per-node context detail.
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
			// A plan was accepted - any earlier rejection this chat recorded no longer
			// describes why the run ended (#1180), same as RecordCallResult's clear.
			inference.ClearPlanRejection(tc.SessionID())

			if err := provisionAndPersist(tc, plan, provision, cache); err != nil {
				return executeResult{}, err
			}

			// Only assignments create_plan/edit_plan added since the last execute
			// call get dispatched - a done one keeps its recorded task_id/result forever.
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
	rec, _, ok, err := loadDagPlan(tc, c)
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
		// Nothing new: don't re-run the pipeline on the unchanged plan (#slice3
		// hang); only "genuinely done" ends the turn - the repeat guard is the backstop.
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

// recordPlanRejection records a rejected plan's reason (cache, metric, judge
// round) - the judge_round save is best-effort and only logged. tripped: the plan loop guard tripped.
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

// provisionAndPersist provisions setup, persists the assembled plan to session
// state (a failed persist surfaces, never drops), caches it selected, emits the plan.
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
	cache.Put(*plan)
	cache.SetSelected(plan.ID)
	// Emits the judged, fully-assembled plan - the resume path
	// (startIncrementalNodeRun) re-emits dag_plan after its own rounds too.
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
		// A dependency pausing earlier aborted runDAGSubset before this
		// node dispatched - leave it untouched so it stays reassignable.
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

// finishExecStep saves the record update, finalizes the answer on a step whose run
// succeeded, and ends the llmagent turn on delivery or pause - a failed step leaves it open.
func finishExecStep(tc agent.Context, c *recordstore.Client, cache *PlanCache, finalize FinalizeAnswerFunc, nodeID string, rec *dag.DagPlanRecord, plan *dag.Plan, results []assignmentResult, stepPaused, stepFailed bool) (executeResult, error) {
	// Only deliver on a step whose own run succeeded - a failed or paused
	// delivering node must not mark the plan done and finalize on garbage.
	// Only a delivering step has an answer to lose; a partial step just reports the cancelled node.
	sinks := stepSinks(*plan, results)
	terminalStopped := rec.Delivery != nil && allStopped(results, sinks)
	delivering := rec.Delivery != nil && !stepPaused && !stepFailed && !terminalStopped
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
		// End the llmagent turn: delivery fired (nothing more to plan) or a node
		// waits on the user; a failed step does NOT end it - the model must react.

		// ponytail: synthesizer node IS the loop-back. Add a caller-side knob when orchestrator needs to reshape.
		tc.Actions().SkipSummarization = true
	}

	slog.Info("execute: plan step ran", "component", "execute", "plan", plan.ID, "ran", len(results), "delivering", delivering, "paused", stepPaused, "failed", stepFailed)
	status := "running"
	switch {
	case delivering:
		status = "delivered"
	case stepPaused:
		status = "paused"
	case terminalStopped:
		status = "stopped"
	}
	return executeResult{Status: status, Results: results}, nil
}

// partitionAssignments splits a plan's assignments into this step's fresh
// dispatches (never run: no task_id yet) and every already-run assignment's
// result, keyed by node id - the seed map a dependent new node's task reads
// its handoff from.
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

// ApplyAssignmentOutcome updates a's TaskID/Result from one attempt's output
// - a fresh step's dispatch (execute.go) or a mid-step HITL resume
// (orchestrator.startIncrementalNodeRun) - and returns its status
// ("done"/"failed"/"paused"). The single place either path decides whether
// an assignment succeeded, so the two can never drift apart on what counts
// as a failure (#slice3 review). paused=false always reports "done" or
// "failed", never "paused" - a resume calls this only after confirming the
// step itself finished (dag.Executor's own paused flag already false).
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
		// Parked on a HITL question, not failed: leave task_id unset so
		// this assignment is still "to run" - see dag.PlanStepSessionID's
		// doc for how a later answer resumes it in place.
		return "paused"
	default:
		a.TaskID = uuid.NewString()
		return "failed"
	}
}

// runningStatus maps a dag_plan record's own status to executeResult's
// vocabulary for the "nothing new to run" short-circuit.
func runningStatus(recStatus string) string {
	if recStatus == "done" {
		return "delivered"
	}
	return "running"
}

// previewText caps a node's raw output to a short preview for the model's
// own turn-loop reading, not the user-facing answer.
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

// DeliverableResults maps each assignment to the result a delivery may use: a stopped one's
// draft reads as empty, so it can never become the delivered answer in any turn; stopped flags those.
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

// ownSinkResults are only the step's own sinks' results: finalize would otherwise also deliver an earlier turn's sinks.
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
func stepSinks(plan dag.Plan, results []assignmentResult) []string {
	ran := make(map[string]bool, len(results))
	for _, r := range results {
		if r.Status != "queued" {
			ran[r.NodeID] = true
		}
	}
	ranSuccessor := map[string]bool{}
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			if ran[n.ID] {
				ranSuccessor[dep] = true
			}
		}
	}
	var sinks []string
	for _, n := range plan.Nodes {
		if ran[n.ID] && !ranSuccessor[n.ID] {
			sinks = append(sinks, n.ID)
		}
	}
	return sinks
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

// presentLeaves are the nodes of among (all of plan's when nil) that have an output and no successor that has one.
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
