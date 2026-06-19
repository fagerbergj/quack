package tools

import (
	"fmt"
	"iter"
	"log/slog"

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
// to the user, caller outputs nothing), "complete" (answer returned for the
// caller to use), or "input_required" (a node paused on request_input and needs
// answers before the DAG can continue — see NodeID/Questions). All guidance lives
// in the tool Description, not this payload. Shared by execute and answer_node.
type executeResult struct {
	Status    string   `json:"status"`              // "delivered" | "complete" | "input_required"
	Answer    string   `json:"answer,omitempty"`    // set only when Status == "complete"
	NodeID    string   `json:"node_id,omitempty"`   // set only when Status == "input_required"
	Questions []string `json:"questions,omitempty"` // set only when Status == "input_required"
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
				"this tool then returns status=\"complete\" with the result in `answer` for you to fold into your reply. " +
				"If a node needs information before it can continue, this returns status=\"input_required\" with `node_id` and `questions`; " +
				"resolve them and call the answer_node tool to resume (see your instructions for how to answer).",
		},
		func(tc agent.ToolContext, a executeArgs) (executeResult, error) {
			plan, ok := cache.Get(a.PlanID)
			if !ok {
				return executeResult{}, fmt.Errorf("execute: unknown plan_id %q — call plan first and pass the plan_id it returns", a.PlanID)
			}
			// Memoised: a repeat execute of a COMPLETED plan reuses the first run's
			// answer instead of re-running the DAG. (A plan that suspended is not
			// cached — the orchestrator resumes it via answer_node, not execute.)
			if answer, cached := cache.Result(a.PlanID); cached {
				slog.Info("plan reusing cached answer", "component", "execute", "plan", a.PlanID, "end_turn", a.EndTurn)
				return deliverOrComplete(tc, a.EndTurn, answer, cache), nil
			}
			nodeOutputs := make(map[string]string)
			res, err := runDAG(tc, executor.Execute(tc, plan, userID, nodeOutputs), plan, nodeOutputs, a.EndTurn, cache)
			if err != nil {
				return executeResult{}, fmt.Errorf("execute: %w", err)
			}
			slog.Info("plan executed", "component", "execute", "plan", a.PlanID, "status", res.Status)
			return res, nil
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

// runDAG drives a DAG event stream (from Execute or Resume): it forwards events to
// the SSE yield seam and classifies the outcome. If a node paused for input it
// returns input_required + that node's questions (the orchestrator answers them and
// resumes via answer_node); otherwise it finalizes via the terminal output, caches
// the answer, and applies end_turn (deliver vs complete). Shared by execute and
// answer_node. Returns an unwrapped error for the caller to prefix.
func runDAG(tc agent.ToolContext, seq iter.Seq2[stream.SSEEvent, error], plan dag.Plan, nodeOutputs map[string]string, endTurn bool, cache *PlanCache) (executeResult, error) {
	yieldFn, hasYield := stream.YieldFromContext(tc)
	waiting, err := drainDAG(seq, func(ev stream.SSEEvent) {
		if hasYield {
			yieldFn(ev)
		}
	})
	if err != nil {
		return executeResult{}, err
	}
	if len(waiting) > 0 {
		// Snapshot the pause so the orchestrator's end-of-turn backstop can resume
		// it deterministically if the model drops the input_required result.
		cache.SetPending(buildPending(plan, nodeOutputs, waiting))
		return executeResult{Status: "input_required", NodeID: waiting[0].NodeID, Questions: waiting[0].Questions}, nil
	}
	cache.ClearPending() // the DAG ran to completion — nothing pending
	answer := TerminalOutput(plan, nodeOutputs)
	if answer == "" {
		return executeResult{}, fmt.Errorf("all nodes completed but produced no output")
	}
	cache.SetResult(plan.ID, answer)
	return deliverOrComplete(tc, endTurn, answer, cache), nil
}

// drainDAG consumes a DAG event stream, forwarding each event to emit, and returns
// every node that paused for input (empty if the DAG ran to completion). Free of
// ToolContext so the classification is unit-testable.
func drainDAG(seq iter.Seq2[stream.SSEEvent, error], emit func(stream.SSEEvent)) ([]stream.NodeWaitingData, error) {
	var waiting []stream.NodeWaitingData
	for ev, err := range seq {
		emit(ev)
		if err != nil {
			return nil, err
		}
		if d, ok := ev.Data.(stream.NodeWaitingData); ok {
			waiting = append(waiting, d)
		}
	}
	return waiting, nil
}

// buildPending turns a suspended run into a resume snapshot: the first waiting node
// is the resume target, the rest stay blocked, and every non-waiting output is a
// completed node to rehydrate downstream.
func buildPending(plan dag.Plan, nodeOutputs map[string]string, waiting []stream.NodeWaitingData) *PendingInput {
	waitingIDs := make(map[string]bool, len(waiting))
	for _, w := range waiting {
		waitingIDs[w.NodeID] = true
	}
	done := make(map[string]string)
	for id, out := range nodeOutputs {
		if !waitingIDs[id] {
			done[id] = out
		}
	}
	primary := waiting[0]
	others := make(map[string]bool)
	for id := range waitingIDs {
		if id != primary.NodeID {
			others[id] = true
		}
	}
	return &PendingInput{Plan: plan, Done: done, Waiting: others, NodeID: primary.NodeID, CallID: primary.CallID}
}

// deliverOrComplete applies end_turn to a finished DAG answer. Deliver (end_turn)
// ends the orchestrator's turn STRUCTURALLY — SkipSummarization makes this tool
// response the final response so the model can't emit a chatty acknowledgement —
// and stashes the answer for the orchestrator to persist as the turn's only
// assistant message. Complete hands the answer back for the caller to fold in.
func deliverOrComplete(tc agent.ToolContext, endTurn bool, answer string, cache *PlanCache) executeResult {
	if endTurn {
		tc.Actions().SkipSummarization = true
		cache.SetDelivered(answer)
		return executeResult{Status: "delivered"}
	}
	return executeResult{Status: "complete", Answer: answer}
}
