package tools

import (
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
	// EndTurn: true when the plan fully answers the user's question (the answer is
	// shown to the user directly and the caller adds nothing); false/omitted when
	// the caller still has work to do with the result. See the tool Description.
	EndTurn bool `json:"end_turn,omitempty"`
}

// executeResult.Status is a bare enum: "delivered" (end_turn=true, answer shown
// to the user, caller outputs nothing) or "complete" (answer returned for the
// caller to use). All guidance lives in the tool Description, not this payload.
type executeResult struct {
	Status string `json:"status"`           // "delivered" | "complete"
	Answer string `json:"answer,omitempty"` // set only when Status == "complete"
}

// ExecPlanIDKey is the session-state key the execute tool stashes the selected
// plan_id under. The orchestrator workflow's execute node reads it and runs the
// DAG in the SAME runner after the llmagent's turn — so a tool (which has no
// sub-scheduler) never runs the DAG, and an empty node can pause the run for
// human steer/cancel natively.
const ExecPlanIDKey = "orch.exec.plan_id"

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
			if _, ok := cache.Get(a.PlanID); !ok {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q — call plan first and pass the plan_id it returns", a.PlanID)
			}
			tc.State().Set(ExecPlanIDKey, a.PlanID)
			// End the llmagent turn structurally so it can't emit a chatty
			// acknowledgement over the execute node's streamed answer.
			// ponytail: fold-into-reply (end_turn=false) dropped in v1 — the execute node
			// always delivers; add a post-execute loop-back node when a plan needs LLM
			// post-processing.
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
