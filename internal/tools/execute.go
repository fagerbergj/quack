package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	Status    string   `json:"status"` // "done" | "failed" | "paused" | "queued" (requested this step but never dispatched - a dependency paused first)
	Summary   string   `json:"summary,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	TaskID    string   `json:"task_id"`
}

// executeResult.Status: "running" while the plan may still grow (this step
// declared no delivery yet - more nodes can follow via edit_plan),
// "delivered" once a step's plan declares delivery and that step's
// delivering node(s) succeeded, "paused" when a node in this step is
// waiting on a question it asked the user - do not call execute/edit_plan
// again until it's answered.
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
func NewExecuteTool(planner *dag.Planner, c *recordstore.Client, cache *PlanCache, provision func(ctx context.Context, userID, chatID string, plan *dag.Plan) error, runStep RunStepFunc, finalize FinalizeAnswerFunc, history []dag.HistoryTurn, message string, attachments []*genai.Part, githubSetup *dag.Setup, allowedKinds []string, workerAsk string, contextItems []dag.ContextItem, planOnly bool, nodeID string, freshnessCheck AssignmentFreshnessFunc) (tool.Tool, error) {
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
			rec, _, ok, err := loadDagPlan(tc, c)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			if !ok {
				return executeResult{}, fmt.Errorf("execute: no plan exists yet - call create_plan first")
			}
			if a.PlanID != "" && a.PlanID != rec.PlanID {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q - the current plan is %q", a.PlanID, rec.PlanID)
			}

			// Nothing new since the last execute call: skip the judge/run/
			// finalize pipeline entirely rather than silently re-running it on
			// an unchanged plan - a model that calls execute again expecting a
			// different answer needs a clear "nothing to do" instead (#slice3
			// review: this was the CI hang - execute doesn't end the turn on
			// its own, so a model re-issuing this with nothing new spun
			// forever). Only "plan is genuinely done" ends the turn here; the
			// repeat guard (orchestrator.go) is the backstop for a model that
			// keeps calling this anyway on a still-partial plan.
			if run, _ := partitionAssignments(rec.Assignments); len(run) == 0 {
				if rec.Status == "done" {
					tc.Actions().SkipSummarization = true
				}
				return executeResult{
					Status:  runningStatus(rec.Status),
					Message: fmt.Sprintf("nothing new to run - plan %s status %q; every assignment has already run", rec.PlanID, rec.Status),
				}, nil
			}

			nodes, err := listDagNodeRecords(tc, c)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
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
			rawNodes, err := dag.AssignmentsToRawNodes(rec.Assignments, nodeAgent, resumedFrom)
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
				reason := err.Error()
				var rejected *dag.PlanRejectedError
				if errors.As(err, &rejected) {
					reason = rejected.Reason
					cache.RecordRejection(reason)
					if _, _, jerr := vetting.SavePlanRejectionJudgeRound(tc, c, nodeID, tc.InvocationID(), reason); jerr != nil {
						slog.Warn("plan rejection judge_round save failed", "component", "execute", "err", jerr)
					}
				}
				inference.RecordPlanRejection(tc.SessionID(), reason)
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			// assemble() mints its own fresh plan.ID - overwrite it with the
			// record's, the id the model actually knows and referenced.
			plan.ID = rec.PlanID
			// The judge and checkReviewDeliverable read plan.Delivery, which
			// assemble() may have resolved from an explicit-but-kindless
			// declaration (see assemble's own doc) - sync it back onto rec so
			// execute's own delivering decision below reads the SAME resolved
			// value the judge just judged, never a stale, less-resolved one.
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
			// A plan was accepted - any earlier rejection this chat recorded no
			// longer describes why the run ended (#1180), same as
			// RecordCallResult's own success clear.
			inference.ClearPlanRejection(tc.SessionID())

			if provision != nil {
				if err := provision(tc, tc.UserID(), tc.SessionID(), plan); err != nil {
					return executeResult{}, fmt.Errorf("execute: %w", err)
				}
			}

			planJSON, err := json.Marshal(*plan)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: marshal plan: %w", err)
			}
			tc.State().Set(ExecPlanKey, string(planJSON))
			cache.Put(*plan)
			cache.SetSelected(plan.ID)

			// Emits the judged, fully-assembled plan - the resume path
			// (startIncrementalNodeRun) re-emits dag_plan again after each
			// of its own rounds, so this is not the plan's only one.
			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				yieldFn(DagPlanEvent(tc, *plan))
			}

			// Only assignments create_plan/edit_plan added since the last execute
			// call get dispatched - a done one keeps its recorded task_id/result forever.
			run, seeded := partitionAssignments(rec.Assignments)
			var outputs map[string]string
			var needsInput map[string]bool
			var started map[string]bool
			if runStep != nil {
				outputs, needsInput, started, err = runStep(tc, *plan, seeded, run)
				if err != nil {
					return executeResult{}, fmt.Errorf("execute: run: %w", err)
				}
			} else {
				// No run function wired (a test double) - treat the whole run set as started.
				started = run
			}
			stepPaused := len(needsInput) > 0

			results := make([]assignmentResult, 0, len(run))
			var stepFailed bool
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
				status := ApplyAssignmentOutcome(&rec.Assignments[i], out, needsInput[nid])
				if status == "failed" {
					stepFailed = true
				}
				arts, _ := nodeArtifactIDs(tc, c, nid)
				results = append(results, assignmentResult{
					NodeID: nid, Status: status, Summary: previewText(out), Artifacts: arts, TaskID: rec.Assignments[i].TaskID,
				})
			}

			// Only deliver when this step's own run actually succeeded - a
			// failed or paused delivering node must not mark the plan done and
			// finalize on garbage; the orchestrator needs the turn to stay
			// open so it can see the failure and react (retry the node,
			// edit_plan, or answer around it).
			delivering := rec.Delivery != nil && !stepPaused && !stepFailed
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
				final := map[string]string{}
				for _, a := range rec.Assignments {
					final[a.NodeID] = a.Result
				}
				cache.SetDelivered(finalize(tc, *plan, final))
			}
			if delivering || stepPaused {
				// End the llmagent turn: either delivery has fired (nothing
				// more to plan) or a node is waiting on the user (calling
				// execute/edit_plan again now would just repeat this step).
				// A failed step deliberately does NOT end the turn - the
				// model needs to see the failure and act on it.
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
			}
			return executeResult{Status: status, Results: results}, nil
		},
	)
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
func ApplyAssignmentOutcome(a *dag.Assignment, output string, paused bool) (status string) {
	switch {
	case strings.TrimSpace(output) != "":
		a.TaskID = uuid.NewString()
		a.Result = output
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

// TerminalOutput: returns the output of the terminal node (no successors). Exported for resume path.
func TerminalOutput(plan dag.Plan, outputs map[string]string) string {
	hasSuccessor := make(map[string]bool, len(plan.Nodes))
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			hasSuccessor[dep] = true
		}
	}
	for _, n := range plan.Nodes {
		if !hasSuccessor[n.ID] {
			if out, ok := outputs[n.ID]; ok {
				return stream.StripThinking(out)
			}
		}
	}
	for i := len(plan.Nodes) - 1; i >= 0; i-- {
		if out, ok := outputs[plan.Nodes[i].ID]; ok {
			return stream.StripThinking(out)
		}
	}
	return ""
}
