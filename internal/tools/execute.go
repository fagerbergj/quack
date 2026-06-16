package tools

import (
	"encoding/json"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

type executeArgs struct {
	Plan string `json:"plan"` // JSON-encoded dag.Plan from the plan tool
}

type executeResult struct {
	Answer string `json:"answer"`
}

// NewExecuteTool returns a tool that runs a DAG plan produced by the plan tool.
// Node events (node_queued, node_start, agent activity, node_done/failed) are
// forwarded up through the orchestrator's SSE stream via the yield context so
// the frontend can render live DAG progress. Returns the final answer.
func NewExecuteTool(executor *dag.Executor, userID string) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name:        "execute",
			Description: "Tool to execute a DAG plan produced by the plan tool. Pass the plan JSON returned by plan. Returns the final synthesized answer.",
		},
		func(tc agent.ToolContext, a executeArgs) (executeResult, error) {
			var plan dag.Plan
			if err := json.Unmarshal([]byte(a.Plan), &plan); err != nil {
				return executeResult{}, fmt.Errorf("execute: parse plan: %w", err)
			}
			yieldFn, hasYield := stream.YieldFromContext(tc)
			nodeOutputs := make(map[string]string)
			for ev, err := range executor.Execute(tc, plan, userID, nodeOutputs) {
				if hasYield {
					yieldFn(ev)
				}
				if err != nil {
					return executeResult{}, fmt.Errorf("execute: %w", err)
				}
			}
			answer := terminalOutput(plan, nodeOutputs)
			if answer == "" {
				return executeResult{}, fmt.Errorf("execute: all nodes completed but produced no output")
			}
			return executeResult{Answer: answer}, nil
		},
	)
}

// terminalOutput returns the output of the plan's terminal node (the one with
// no successors). Falls back to the last node in slice order.
func terminalOutput(plan dag.Plan, outputs map[string]string) string {
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
