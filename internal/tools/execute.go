package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

type executeArgs struct {
	PlanID string `json:"plan_id"` // the plan_id returned by the plan tool
}

// executeResult.Status: always "delivered".
type executeResult struct {
	Status string `json:"status"` // "delivered"
}

// ExecPlanKey: session-state key for the selected plan (full JSON), so a retry finds it in persisted session.
const ExecPlanKey = "orch.exec.plan"

// NewExecuteTool: validates plan_id, provisions its Setup, selects it, ends
// the llmagent's turn. provision: eager clone+checkout of plan.Setup (nil-safe
// no-op if unset) - runs HERE, synchronously, so a clone failure is this
// tool's own error result (the model sees it and can revise the plan) rather
// than a run-time abort deep inside RunPlanAsGraph (#848).
func NewExecuteTool(cache *PlanCache, provision func(ctx context.Context, userID, chatID string, plan *dag.Plan) error) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to execute a DAG plan produced by the plan tool. Pass the plan_id returned by plan. " +
				"The plan's answer is shown to the user directly; after calling execute you must output nothing " +
				"further - no acknowledgement, no restatement, and never say a specialist will respond (the work is already done).",
		},
		func(tc agent.Context, a executeArgs) (executeResult, error) {
			plan, ok := cache.Get(a.PlanID)
			if !ok {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q - call plan first and pass the plan_id it returns", a.PlanID)
			}
			if provision != nil {
				if err := provision(tc, tc.UserID(), tc.SessionID(), &plan); err != nil {
					return executeResult{}, fmt.Errorf("execute: %w", err)
				}
			}
			planJSON, err := json.Marshal(plan)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: marshal plan: %w", err)
			}
			tc.State().Set(ExecPlanKey, string(planJSON))
			cache.SetSelected(a.PlanID)
			// End the llmagent turn to prevent chattering over the streamed answer.
			// ponytail: synthesizer node IS the loop-back. Add a caller-side knob when orchestrator needs to reshape.
			tc.Actions().SkipSummarization = true
			slog.Info("plan selected for execution", "component", "execute", "plan", a.PlanID)
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
