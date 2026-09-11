package tools

import (
	"fmt"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
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
// when given. Unmentioned assignments are left exactly as they are. nodeID
// is the AUTHORING lineage id stamped on the saved records (e.g.
// "orchestrator") - unrelated to a dag_node's own node_id. onAssignment,
// when non-nil, stamps assignment.meta.<extension> - whichever active
// extension supplies it, keyed by its own name (e.g. "github").
func NewEditPlanTool(c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc) (tool.Tool, error) {
	schema, err := assignmentInputSchema[editPlanArgs]()
	if err != nil {
		return nil, fmt.Errorf("edit_plan: %w", err)
	}
	return functiontool.New[editPlanArgs, planUpsertResult](
		functiontool.Config{
			Name:        "edit_plan",
			InputSchema: schema,
			Description: "Tool to change the chat's current plan: upsert assignments (same shape as create_plan - " +
				"`agent` hires a new node, `node_id` from list_nodes reassigns one) keyed by node_id, drop " +
				"assignments by node_id with `remove`, and/or update `setup`/`delivery`. Assignments not named " +
				"here are left exactly as they are. Call with at least one of assignments/remove/setup/delivery set - " +
				"a call that changes nothing is rejected, it is not a valid way to re-read the plan (use list_nodes). " +
				"When this dispatch already came with a repo/base_ref (a GitHub trigger), a `setup.repo`/`setup.base_ref` " +
				"override must match it exactly - omit them to keep the trigger's own values. Errors name the field and " +
				"the fix: unknown agent, unknown depends_on id, a dependency cycle, an empty task, a node currently " +
				"running, the same node_id twice in one call, a `remove` id not in the current plan, a setup override " +
				"that disagrees with the trigger, hiring an agent whose only deliverable this dispatch does not allow, " +
				"a plan that already delivered, or a call with nothing to change. Call after create_plan to correct or " +
				"extend a plan before execute; call list_nodes first to reuse a node instead of hiring a new one.",
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
			if current.Status == "done" {
				return planUpsertResult{}, fmt.Errorf("edit_plan: plan %q already delivered - it's finished, not editable; start a new plan for further work", current.PlanID)
			}
			if len(a.Assignments) == 0 && len(a.Remove) == 0 && a.Setup == nil && a.Delivery == nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: nothing to change - got plan_id %q with no assignments, remove, setup, "+
					"or delivery; edit_plan accepts assignments (each with node_id or agent), remove, setup, and/or delivery - set at least one", a.PlanID)
			}
			if err := dag.ValidateSetupOverride(a.Setup, githubSetup); err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}

			remaining, err := removeAssignments(current.Assignments, a.Remove)
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}

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
				upserts, minted, err = upsertNodes(a.Assignments, existingNodes, nodeIsRunning, tc.SessionID(), allowedKinds)
				if err != nil {
					return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
				}
				for _, n := range minted {
					nodeAgent[n.NodeID] = n.Agent
				}
				stampAssignmentMeta(tc, current.PlanID, nodeAgent, upserts, onAssignment)
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

			// dag_plan (which validates) saves before any minted dag_node, so a
			// rejected call leaves no orphan "hired" node behind for list_nodes.
			now := time.Now().UTC()
			lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
			if _, _, err := c.SaveStructured(tc, "dag_plan", rec, "", lineage); err != nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
			}
			for _, n := range minted {
				nodeLineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
				if _, _, err := c.SaveStructured(tc, "dag_node", n, n.NodeID, nodeLineage); err != nil {
					// The dag_plan revision above already saved - a bare error here
					// leaves a live plan referencing a node list_nodes won't show yet.
					return planUpsertResult{}, fmt.Errorf("edit_plan: save dag_node %s: %w; "+
						"the plan was already saved - call edit_plan again to replace it", n.NodeID, err)
				}
			}

			// No dag_plan event here: node cards must not appear (or gain new
			// ones) until execute actually dispatches them - execute's own
			// DagPlanEvent is the only dag_plan emission (list_nodes shows the
			// draft's current assignments as text meanwhile).
			return planUpsertResult{
				PlanID: rec.PlanID, Assignments: toAssignmentOutputs(rec.Assignments, nodeAgent),
				Setup: rec.Setup, Delivery: rec.Delivery, Summary: summarizePlanRecord(rec, nodeAgent),
			}, nil
		},
	)
}

// removeAssignments drops every assignment whose node_id is in remove.
// Errors on a remove id naming no current assignment (a typo must not
// silently no-op) or one that already ran (dag.Assignment.TaskID != "") -
// a done assignment's task_id/result is load-bearing (execute's seed map for
// any dependent added later), so removing it is a one-line error, not a
// silent history rewrite.
func removeAssignments(assignments []dag.Assignment, remove []string) ([]dag.Assignment, error) {
	if len(remove) == 0 {
		return assignments, nil
	}
	drop := make(map[string]bool, len(remove))
	for _, id := range remove {
		drop[id] = true
	}
	byID := make(map[string]dag.Assignment, len(assignments))
	for _, a := range assignments {
		byID[a.NodeID] = a
	}
	for _, id := range remove {
		a, present := byID[id]
		if !present {
			return nil, fmt.Errorf("remove: unknown node id %q - not in the current plan", id)
		}
		if a.TaskID != "" {
			return nil, fmt.Errorf("remove: %q already ran - editing or removing a done assignment is not allowed", id)
		}
	}
	out := make([]dag.Assignment, 0, len(assignments))
	for _, a := range assignments {
		if !drop[a.NodeID] {
			out = append(out, a)
		}
	}
	return out, nil
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
