package tools

import (
	"fmt"
	"log"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

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

// NewExecuteTool returns a tool that runs a DAG plan produced by the plan tool.
// The plan is looked up by ID from cache (the same cache the plan tool wrote to)
// rather than parsed from a model-relayed JSON blob — the model only passes the
// short plan_id, so no nodes can be lost in transit. Node events (node_queued,
// node_start, agent activity, node_done/failed) are forwarded up through the
// orchestrator's SSE stream via the yield context so the frontend can render
// live DAG progress. Returns the final answer.
func NewExecuteTool(executor *dag.Executor, cache *PlanCache, userID string) (tool.Tool, error) {
	return functiontool.New[executeArgs, executeResult](
		functiontool.Config{
			Name: "execute",
			Description: "Tool to execute a DAG plan produced by the plan tool. Pass the plan_id returned by plan. " +
				"Set end_turn=true whenever the plan can fully answer the user's question on its own (the usual case): " +
				"the answer is shown to the user directly, this tool returns status=\"delivered\" with no answer text, " +
				"and you must then output nothing further — no acknowledgement, no restatement, and never say a specialist will respond (the work is already done). " +
				"Set end_turn=false (or omit it) only when you still have work to do after the plan runs — combining its result with other information or reshaping it yourself: " +
				"this tool then returns status=\"complete\" with the result in `answer` for you to fold into your reply.",
		},
		func(tc agent.ToolContext, a executeArgs) (executeResult, error) {
			plan, ok := cache.Get(a.PlanID)
			if !ok {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q — call plan first and pass the plan_id it returns", a.PlanID)
			}
			// Memoised: a repeat execute of the same plan reuses the first run's
			// answer instead of re-running the DAG (minutes + tokens). Only the
			// (cheap) end_turn handling below re-runs.
			answer, cached := cache.Result(a.PlanID)
			if cached {
				log.Printf("execute: plan %s reusing cached answer (end_turn=%v)", a.PlanID, a.EndTurn)
			} else {
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
				answer = terminalOutput(plan, nodeOutputs)
				if answer == "" {
					return executeResult{}, fmt.Errorf("execute: all nodes completed but produced no output")
				}
				cache.SetResult(a.PlanID, answer)
				log.Printf("execute: plan %s end_turn=%v answer_len=%d", a.PlanID, a.EndTurn, len(answer))
			}
			if a.EndTurn {
				// Deliver: the answer already streamed to the user. End the
				// orchestrator's turn STRUCTURALLY — SkipSummarization makes this tool
				// response the final response, so the runner never calls the model
				// again and it cannot emit a chatty acknowledgement (relying on the
				// prompt to stay silent proved unreliable). Withhold the answer text so
				// it can't echo, and stash it so the orchestrator persists it as the
				// turn's (only) assistant message.
				tc.Actions().SkipSummarization = true
				cache.SetDelivered(answer)
				return executeResult{Status: "delivered"}, nil
			}
			// Caller still has work to do: hand the answer back to fold into its reply.
			return executeResult{Status: "complete", Answer: answer}, nil
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
