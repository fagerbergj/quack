package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

type answerNodeArgs struct {
	PlanID  string   `json:"plan_id"`
	NodeID  string   `json:"node_id"`
	Answers []string `json:"answers"` // one per question, in the order asked
	// EndTurn behaves exactly like execute's: true delivers the final answer to the
	// user; false/omitted hands it back for you to fold into your reply.
	EndTurn bool `json:"end_turn,omitempty"`
}

// NewAnswerNodeTool returns the tool that resumes a DAG node paused on request_input.
// The orchestrator calls it after execute/answer_node returned input_required, with
// the answers it resolved (from its own context, or the user's via get_user_choice).
// The plan is reconstructed from the store (the in-memory PlanCache is gone once the
// orchestrator's turn ended at a get_user_choice pause), so resume survives a
// restart. The resumed DAG runs through the same classifier as execute and may pause
// again (another input_required) or finish (delivered/complete).
func NewAnswerNodeTool(executor *dag.Executor, cache *PlanCache, st *store.Store, appName, userID, sessionID string) (tool.Tool, error) {
	return functiontool.New[answerNodeArgs, executeResult](
		functiontool.Config{
			Name: "answer_node",
			Description: "Resume a DAG node that paused for input (after execute/answer_node returned " +
				"status=\"input_required\"). Pass plan_id and node_id from that result and `answers` — " +
				"one answer per question, in the order asked. Set end_turn the same way as execute " +
				"(true to deliver the final answer to the user, false to fold it into your reply). " +
				"The DAG continues and may pause again (another input_required) or finish.",
		},
		func(tc agent.ToolContext, a answerNodeArgs) (executeResult, error) {
			plan, done, waiting, callID, err := loadResumeState(tc, st, appName, userID, sessionID, a.PlanID, a.NodeID)
			if err != nil {
				return executeResult{}, fmt.Errorf("answer_node: %w", err)
			}
			content := &genai.Content{Role: "user", Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					ID:       callID,
					Name:     RequestInputToolName,
					Response: map[string]any{RequestInputAnswerKey: a.Answers},
				},
			}}}
			nodeOutputs := make(map[string]string)
			res, err := runDAG(tc, executor.Resume(tc, plan, userID, nodeOutputs, done, waiting, a.NodeID, content), plan, nodeOutputs, a.EndTurn, cache)
			if err != nil {
				return executeResult{}, fmt.Errorf("answer_node: %w", err)
			}
			return res, nil
		},
	)
}

// loadResumeState reconstructs the executable plan + resume maps for a paused node
// from the store: the done-node outputs (rehydrated downstream), the set of OTHER
// still-waiting nodes (kept blocked), and the target node's open request_input call
// ID. Errors if the plan/node isn't found or the node isn't waiting.
func loadResumeState(ctx context.Context, st *store.Store, appName, userID, sessionID, planID, nodeID string) (dag.Plan, map[string]string, map[string]bool, string, error) {
	turns, err := st.GetTurnsWithContent(ctx, appName, userID, sessionID)
	if err != nil {
		return dag.Plan{}, nil, nil, "", err
	}
	var tc *store.TurnContent
	for i := range turns {
		if turns[i].Plan != nil && turns[i].Plan.ID == planID {
			tc = &turns[i]
			break
		}
	}
	if tc == nil {
		return dag.Plan{}, nil, nil, "", fmt.Errorf("plan %q not found", planID)
	}
	var target *store.DagNode
	done := map[string]string{}
	waiting := map[string]bool{}
	for i := range tc.Nodes {
		n := &tc.Nodes[i]
		switch {
		case n.NodeID == nodeID:
			target = n
		case n.Status == "done":
			done[n.NodeID] = n.Output
		case n.Status == "waiting":
			waiting[n.NodeID] = true
		}
	}
	if target == nil {
		return dag.Plan{}, nil, nil, "", fmt.Errorf("node %q not found in plan %q", nodeID, planID)
	}
	if target.Status != "waiting" {
		return dag.Plan{}, nil, nil, "", fmt.Errorf("node %q is not waiting for input", nodeID)
	}
	plan, err := reconstructPlan(planID, nodeID, tc)
	if err != nil {
		return dag.Plan{}, nil, nil, "", err
	}
	return plan, done, waiting, target.WaitingCallID, nil
}

// reconstructPlan rebuilds an executable dag.Plan from persisted state: the wire
// DagPlanData (node id/agent/task/deps) plus the turn's user text. Rubric and
// History are unused at execution; attachments are not re-threaded (resuming a media
// node downstream of a pause is out of scope). Errors if the stored plan JSON can't
// be parsed or doesn't contain the node being resumed — otherwise a bad plan would
// silently resume nothing.
func reconstructPlan(planID, nodeID string, tc *store.TurnContent) (dag.Plan, error) {
	var wire stream.DagPlanData
	if err := json.Unmarshal([]byte(tc.Plan.PlanJSON), &wire); err != nil {
		return dag.Plan{}, fmt.Errorf("reconstruct plan %s: %w", planID, err)
	}
	nodes := make([]dag.Node, len(wire.Nodes))
	found := false
	for i, n := range wire.Nodes {
		nodes[i] = dag.Node{ID: n.ID, AgentName: n.Agent, Task: n.Task, DependsOn: n.DependsOn}
		if n.ID == nodeID {
			found = true
		}
	}
	if !found {
		return dag.Plan{}, fmt.Errorf("reconstruct plan %s: node %q not in stored plan", planID, nodeID)
	}
	return dag.Plan{ID: planID, UserMessage: tc.UserText, Nodes: nodes}, nil
}
