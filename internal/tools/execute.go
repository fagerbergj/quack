package tools

import (
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

// executeResult.Status is always "delivered": the plan's answer streams straight to
// the user from the execute node and the orchestrator stays silent.
type executeResult struct {
	Status string `json:"status"` // "delivered"
}

// ExecPlanKey is the session-state key the execute tool stashes the selected plan
// (full JSON) under. The orchestrator workflow's execute node reads it and runs the
// DAG in the SAME runner after the llmagent's turn — so a tool (which has no
// sub-scheduler) never runs the DAG. Storing the whole plan (not just its id) means
// a retry of a failed node finds it in the persisted session, where the
// per-run plan cache no longer exists.
const ExecPlanKey = "orch.exec.plan"

// NewExecuteTool returns the execute tool. In the single-runner model it does not
// run the DAG (a tool context has no sub-scheduler); it validates the plan_id and
// selects it for the workflow's execute node, then ends the llmagent's turn so it
// can't chatter over the streamed answer.
func NewExecuteTool(cache *PlanCache) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to execute a DAG plan produced by the plan tool. Pass the plan_id returned by plan. " +
				"The plan's answer is shown to the user directly; after calling execute you must output nothing " +
				"further — no acknowledgement, no restatement, and never say a specialist will respond (the work is already done).",
		},
		func(tc agent.Context, a executeArgs) (executeResult, error) {
			plan, ok := cache.Get(a.PlanID)
			if !ok {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q — call plan first and pass the plan_id it returns", a.PlanID)
			}
			planJSON, err := json.Marshal(plan)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: marshal plan: %w", err)
			}
			tc.State().Set(ExecPlanKey, string(planJSON))
			cache.SetSelected(a.PlanID)
			// End the llmagent turn structurally so it can't emit a chatty
			// acknowledgement over the execute node's streamed answer.
			// ponytail: the plan's synthesizer node IS the loop-back — it folds the
			// specialist outputs into the final answer. Full fold-into-reply just needs the
			// orchestrator's context (conversation history + the user's exact framing)
			// threaded into that node; end_turn=false is a no-op for now (always deliver).
			tc.Actions().SkipSummarization = true
			slog.Info("plan selected for execution", "component", "execute", "plan", a.PlanID)
			return executeResult{Status: "delivered"}, nil
		},
	)
}

// TerminalOutput returns the output of the plan's terminal node (the one with
// no successors). Exported for the resume path. Falls back to the last node in slice order.
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
