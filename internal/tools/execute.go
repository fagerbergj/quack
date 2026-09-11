package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

type executeArgs struct {
	PlanID string `json:"plan_id"` // the plan_id create_plan/edit_plan returned
}

// executeResult.Status: always "delivered".
type executeResult struct {
	Status string `json:"status"` // "delivered"
}

// ExecPlanKey: session-state key for the selected plan (full JSON), so a retry finds it in persisted session.
const ExecPlanKey = "orch.exec.plan"

// executeRejectionCap bounds how many times the plan judge may reject a plan
// within one turn before execute refuses to run it again itself, without
// ever calling the judge. A model that keeps calling create_plan/edit_plan/
// execute in one long-running invocation grows the conversation by a full
// plan-judge round each time; left uncapped, that growth can outrun the
// model's context window before the orchestrator's own post-invocation
// exhaustion check (a separate, turn-level guard) ever gets a chance to run.
// Mirrors orchestrator.minRejectionsForExhaustion (a different package, same
// number, cheap enough to keep in sync by eye rather than share).
const executeRejectionCap = 2

// NewExecuteTool: reads the chat's current dag_plan record and its
// dag_node agents, runs planner.Build (unknown agent, cycle,
// review-deliverable, review-fanout, the plan judge, against this
// conversation's history/message), provisions Setup, and selects the plan
// for the executor. A rejection surfaces as a tool error - edit_plan and
// call execute again.
func NewExecuteTool(planner *dag.Planner, c *recordstore.Client, cache *PlanCache, provision func(ctx context.Context, userID, chatID string, plan *dag.Plan) error, history []dag.HistoryTurn, message string, attachments []*genai.Part, githubSetup *dag.Setup, allowedKinds []string, workerAsk string, contextItems []dag.ContextItem, planOnly bool) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to execute the plan create_plan/edit_plan built. Pass the plan_id it returned. " +
				"Reads the plan's latest revision, runs the plan judge against it and this conversation, and - " +
				"if accepted - runs it; a rejection comes back as an error naming what to fix, so edit_plan and " +
				"call execute again. After 2 rejections in one turn, execute refuses to try a third time - stop " +
				"and answer the user directly explaining what's blocking a workable plan, do not keep editing. " +
				"The plan's answer is shown to the user directly; after calling execute you " +
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
			if count, reason := cache.Rejections(); count >= executeRejectionCap {
				return executeResult{}, fmt.Errorf("execute: the plan judge already rejected %d plan(s) this turn (most recently: %s) - "+
					"stop calling create_plan/edit_plan/execute and answer the user directly, explaining what is blocking a workable plan", count, reason)
			}

			nodes, err := listDagNodeRecords(tc, c)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			nodeAgent := make(map[string]string, len(nodes))
			for _, n := range nodes {
				nodeAgent[n.NodeID] = n.Agent
			}
			rawNodes, err := dag.AssignmentsToRawNodes(rec.Assignments, nodeAgent)
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
