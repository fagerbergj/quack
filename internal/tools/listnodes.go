package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/recordstore"
)

type listNodesArgs struct{}

// nodeSummary is one entry of list_nodes' response.
type nodeSummary struct {
	NodeID    string `json:"node_id"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
	ContextID string `json:"context_id,omitempty"`
	LastTask  string `json:"last_task,omitempty"`
	// LastTaskID: the A2A task_id execute recorded for this node's current
	// assignment (dag.Assignment.TaskID) - lets a caller correlate this
	// dispatch with its A2A task without re-deriving it.
	LastTaskID string   `json:"last_task_id,omitempty"`
	Artifacts  []string `json:"artifacts,omitempty"`
	// Resumable: true when naming this node_id in create_plan/edit_plan
	// continues its own session with the new task as its next turn, instead
	// of a "currently running"/"hasn't run yet" rejection. Reason always says why (or why not).
	Resumable bool   `json:"resumable"`
	Reason    string `json:"reason"`
}

// NewListNodesTool: this chat's nodes - people already hired to do an
// agent's job, with id, agent, live status, A2A context_id, the first line
// of their latest assignment, the artifacts they've written, and whether
// reassigning them resumes their own session. nodeIsRunning is nil-safe.
func NewListNodesTool(c *recordstore.Client, nodeIsRunning func(nodeID string) bool) (tool.Tool, error) {
	return functiontool.New[listNodesArgs, string](
		functiontool.Config{
			Name: "list_nodes",
			Description: "List this chat's nodes: people already hired to do an agent's job, with their id, " +
				"agent, live status, A2A context_id, the first line and task_id of their current assignment, " +
				"the artifacts they've written, and `resumable` (true when this node has finished a run, so " +
				"naming its node_id in create_plan/edit_plan continues that same session with the new task). " +
				"Call before create_plan/edit_plan to reuse an existing node instead of hiring a new one for the same job.",
		},
		func(ctx agent.Context, _ listNodesArgs) (string, error) {
			summaries, err := buildNodeSummaries(ctx, c, nodeIsRunning)
			if err != nil {
				return "", fmt.Errorf("list_nodes: %w", err)
			}
			if len(summaries) == 0 {
				return "(no nodes hired yet)", nil
			}
			b, err := json.MarshalIndent(summaries, "", "  ")
			if err != nil {
				return "", fmt.Errorf("list_nodes: %w", err)
			}
			return string(b), nil
		},
	)
}

func buildNodeSummaries(ctx context.Context, c *recordstore.Client, nodeIsRunning func(nodeID string) bool) ([]nodeSummary, error) {
	nodes, err := listDagNodeRecords(ctx, c)
	if err != nil {
		return nil, err
	}
	lastTask := map[string]string{}
	lastTaskID := map[string]string{}
	if plan, _, ok, _ := loadDagPlan(ctx, c); ok {
		for _, a := range plan.Assignments {
			lastTask[a.NodeID] = firstLine(a.Task)
			lastTaskID[a.NodeID] = a.TaskID
		}
	}
	out := make([]nodeSummary, 0, len(nodes))
	for _, n := range nodes {
		status := string(n.Status)
		if nodeIsRunning != nil && nodeIsRunning(n.NodeID) {
			status = "running"
		}
		arts, _ := nodeArtifactIDs(ctx, c, n.NodeID)
		resumable, reason := n.Resumable()
		if nodeIsRunning != nil && nodeIsRunning(n.NodeID) {
			// Live truth outranks the stored status snapshot.
			resumable, reason = false, "currently running"
		}
		out = append(out, nodeSummary{
			NodeID: n.NodeID, Agent: n.Agent, Status: status, ContextID: n.ContextID,
			LastTask: lastTask[n.NodeID], LastTaskID: lastTaskID[n.NodeID], Artifacts: arts,
			Resumable: resumable, Reason: reason,
		})
	}
	return out, nil
}
