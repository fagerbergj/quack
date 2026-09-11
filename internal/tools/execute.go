package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
// instead of resuming; nil skips the check (always fresh).
type AssignmentFreshnessFunc func(ctx agent.Context, a dag.Assignment) (fresh bool, reason string)

type executeArgs struct {
	PlanID string `json:"plan_id"` // the plan_id create_plan/edit_plan returned
}

// executeResult.Status: always "delivered".
type executeResult struct {
	Status string `json:"status"` // "delivered"
}

// ExecPlanKey: session-state key for the selected plan (full JSON), so a retry finds it in persisted session.
const ExecPlanKey = "orch.exec.plan"

// NewExecuteTool: reads the chat's current dag_plan record and its
// dag_node agents, runs planner.Build (unknown agent, cycle,
// review-deliverable, review-fanout, the plan judge, against this
// conversation's history/message), provisions Setup, and selects the plan
// for the executor. A rejection surfaces as a tool error - edit_plan and
// call execute again, and is also recorded as a judge_round anchored to
// nodeID (the authoring lineage id) so the artifact panel shows it. Every
// assignment naming a terminal (already-run) node id resumes that node's
// own session, unless freshnessCheck says it's stale.
func NewExecuteTool(planner *dag.Planner, c *recordstore.Client, cache *PlanCache, provision func(ctx context.Context, userID, chatID string, plan *dag.Plan) error, history []dag.HistoryTurn, message string, attachments []*genai.Part, githubSetup *dag.Setup, allowedKinds []string, workerAsk string, contextItems []dag.ContextItem, planOnly bool, nodeID string, freshnessCheck AssignmentFreshnessFunc) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to execute the plan create_plan/edit_plan built. Pass the plan_id it returned. " +
				"Reads the plan's latest revision, runs the plan judge against it and this conversation, and - " +
				"if accepted - runs it; a rejection comes back as an error naming what to fix, so edit_plan and " +
				"call execute again. The plan's answer is shown to the user directly; after calling execute you " +
				"must output nothing further - no acknowledgement, no restatement, and never say a specialist " +
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
					if fresh, reason := freshnessCheck(tc, a); !fresh {
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

			// Stamps the plan record's own lifecycle past "planned" (nothing
			// else advances it - a later slice owns "done") and records each
			// assignment's dispatch task_id. Best-effort: a store hiccup here
			// must not block a plan the judge already accepted.
			running := rec
			running.Status = "running"
			for i := range running.Assignments {
				running.Assignments[i].TaskID = uuid.NewString()
			}
			runLineage := recordstore.Lineage{NodeID: nodeID, Author: "system", SavedAt: time.Now().UTC()}
			if _, _, serr := c.SaveStructured(tc, "dag_plan", running, "", runLineage); serr != nil {
				slog.Warn("execute: dag_plan running/task_id update failed", "component", "execute", "err", serr)
			}

			planJSON, err := json.Marshal(*plan)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: marshal plan: %w", err)
			}
			tc.State().Set(ExecPlanKey, string(planJSON))
			cache.Put(*plan)
			cache.SetSelected(plan.ID)

			// Re-emits dag_plan with the judged, fully-assembled shape - create_plan/edit_plan already sent an earlier draft.
			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				yieldFn(DagPlanEvent(tc, *plan))
			}
			emitPlanEvent(tc, plan)

			// End the llmagent turn to prevent chattering over the streamed answer.
			// ponytail: synthesizer node IS the loop-back. Add a caller-side knob when orchestrator needs to reshape.
			tc.Actions().SkipSummarization = true
			slog.Info("plan selected for execution", "component", "execute", "plan", plan.ID)
			return executeResult{Status: "delivered"}, nil
		},
	)
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
