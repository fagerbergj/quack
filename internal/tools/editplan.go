package tools

import (
	"fmt"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
)

type editPlanArgs struct {
	PlanID      string            `json:"plan_id"`
	Assignments []assignmentInput `json:"assignments,omitempty"`
	Remove      []string          `json:"remove,omitempty"`
	Setup       *dag.Setup        `json:"setup,omitempty"`
	Delivery    *dag.Delivery     `json:"delivery,omitempty"`
}

// NewEditPlanTool: upserts assignments (keyed by node_id, same shape as
// create_plan - agent hires, node_id reassigns) into the chat's current
// plan, drops assignments named in `remove`, and updates setup/delivery
// when given. Unmentioned assignments are left exactly as they are.
func NewEditPlanTool(c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool) (tool.Tool, error) {
	return functiontool.New[editPlanArgs, planUpsertResult](
		functiontool.Config{
			Name: "edit_plan",
			Description: "Tool to change the chat's current plan: upsert assignments (same shape as create_plan - " +
				"`agent` hires a new node, `node_id` from list_nodes reassigns one) keyed by node_id, drop " +
				"assignments by node_id with `remove`, and/or update `setup`/`delivery`. Assignments not named " +
				"here are left exactly as they are. Errors name the field and the fix: unknown agent, unknown " +
				"depends_on id, a dependency cycle, an empty task, or a node currently running. Call after " +
				"create_plan to correct or extend a plan before execute; call list_nodes first to reuse a node " +
				"instead of hiring a new one.",
		},
		func(tc agent.Context, a editPlanArgs) (planUpsertResult, error) {
			current, _, ok, err := loadDagPlan(tc, c)
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}
			if !ok {
				return planUpsertResult{}, fmt.Errorf("edit_plan: no plan exists yet in this chat - call create_plan first")
			}
			if a.PlanID != "" && a.PlanID != current.PlanID {
				return planUpsertResult{}, fmt.Errorf("edit_plan: plan_id %q is stale - the current plan is %q", a.PlanID, current.PlanID)
			}

			remaining := removeAssignments(current.Assignments, a.Remove)

			existingNodes, err := listDagNodeRecords(tc, c)
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}
			nodeAgent := map[string]string{}
			for _, n := range existingNodes {
				nodeAgent[n.NodeID] = n.Agent
			}

			var upserts []dag.Assignment
			var minted []dag.DagNodeRecord
			if len(a.Assignments) > 0 {
				upserts, minted, err = upsertNodes(a.Assignments, existingNodes, nodeIsRunning, tc.SessionID())
				if err != nil {
					return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
				}
				for _, n := range minted {
					nodeAgent[n.NodeID] = n.Agent
				}
			}

			rec := current
			rec.Assignments = mergeAssignments(remaining, upserts)
			if a.Setup != nil {
				rec.Setup = a.Setup
			}
			if githubSetup != nil {
				s := *githubSetup
				rec.Setup = &s
			}
			if a.Delivery != nil {
				rec.Delivery = a.Delivery
			}

			now := time.Now().UTC()
			for _, n := range minted {
				lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
				if _, _, err := c.SaveStructured(tc, "dag_node", n, n.NodeID, lineage); err != nil {
					return planUpsertResult{}, fmt.Errorf("edit_plan: save dag_node %s: %w", n.NodeID, err)
				}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
			if _, _, err := c.SaveStructured(tc, "dag_plan", rec, "", lineage); err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}

			if yieldFn, ok := stream.YieldFromContext(tc); ok {
				yieldFn(planRecordEvent(tc, rec, nodeAgent))
			}
			return planUpsertResult{
				PlanID: rec.PlanID, Assignments: toAssignmentOutputs(rec.Assignments, nodeAgent),
				Setup: rec.Setup, Delivery: rec.Delivery, Summary: summarizePlanRecord(rec, nodeAgent),
			}, nil
		},
	)
}

// removeAssignments drops every assignment whose node_id is in remove.
func removeAssignments(assignments []dag.Assignment, remove []string) []dag.Assignment {
	if len(remove) == 0 {
		return assignments
	}
	drop := make(map[string]bool, len(remove))
	for _, id := range remove {
		drop[id] = true
	}
	out := make([]dag.Assignment, 0, len(assignments))
	for _, a := range assignments {
		if !drop[a.NodeID] {
			out = append(out, a)
		}
	}
	return out
}

// mergeAssignments upserts upserts into current by node_id, preserving
// current's order for an assignment that already existed and appending a
// brand-new one at the end.
func mergeAssignments(current, upserts []dag.Assignment) []dag.Assignment {
	byID := make(map[string]dag.Assignment, len(upserts))
	for _, u := range upserts {
		byID[u.NodeID] = u
	}
	seen := make(map[string]bool, len(current))
	out := make([]dag.Assignment, 0, len(current)+len(upserts))
	for _, c := range current {
		if u, ok := byID[c.NodeID]; ok {
			out = append(out, u)
		} else {
			out = append(out, c)
		}
		seen[c.NodeID] = true
	}
	for _, u := range upserts {
		if !seen[u.NodeID] {
			out = append(out, u)
		}
	}
	return out
}
