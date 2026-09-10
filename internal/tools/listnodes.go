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
	NodeID    string   `json:"node_id"`
	Agent     string   `json:"agent"`
	Status    string   `json:"status"`
	LastTask  string   `json:"last_task,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// NewListNodesTool: this chat's nodes - people already hired to do an
// agent's job, with id, agent, live status, the first line of their latest
// assignment, and the artifacts they've written. nodeIsRunning is nil-safe.
func NewListNodesTool(c *recordstore.Client, nodeIsRunning func(nodeID string) bool) (tool.Tool, error) {
	return functiontool.New[listNodesArgs, string](
		functiontool.Config{
			Name: "list_nodes",
			Description: "List this chat's nodes: people already hired to do an agent's job, with their id, " +
				"agent, live status, the first line of their current assignment, and the artifacts they've " +
				"written. Call before create_plan/edit_plan to reuse an existing node instead of hiring a new " +
				"one for the same job.",
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
	if plan, _, ok, _ := loadDagPlan(ctx, c); ok {
		for _, a := range plan.Assignments {
			lastTask[a.NodeID] = firstLine(a.Task)
		}
	}
	out := make([]nodeSummary, 0, len(nodes))
	for _, n := range nodes {
		status := string(n.Status)
		if nodeIsRunning != nil && nodeIsRunning(n.NodeID) {
			status = "running"
		}
		arts, _ := nodeArtifactIDs(ctx, c, n.NodeID)
		out = append(out, nodeSummary{NodeID: n.NodeID, Agent: n.Agent, Status: status, LastTask: lastTask[n.NodeID], Artifacts: arts})
	}
	return out, nil
}
