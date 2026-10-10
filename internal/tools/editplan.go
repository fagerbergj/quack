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

// NewEditPlanTool leaves unmentioned assignments untouched. nodeID is the authoring lineage id
// (e.g. "orchestrator"), unrelated to a dag_node's node_id.
func NewEditPlanTool(c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc, agents []string) (tool.Tool, error) {
	schema, err := assignmentInputSchema[editPlanArgs](githubSetup, agents)
	if err != nil {
		return nil, fmt.Errorf("edit_plan: %w", err)
	}
	changeList := "assignments/remove/setup/delivery"
	setupDesc := ", and/or update `setup`/`delivery`"
	setupOverride := "a setup override that disagrees with the trigger, "
	if githubSetup != nil {
		changeList = "assignments/remove/delivery"
		setupDesc = ", and/or update `delivery`; `setup` isn't offered here - this dispatch's repo/base_ref is fixed by its trigger"
		setupOverride = ""
	}
	return functiontool.New[editPlanArgs, planUpsertResult](
		functiontool.Config{
			Name:        "edit_plan",
			InputSchema: schema,
			Description: "Tool to change the chat's current plan: upsert assignments (same shape as create_plan - " +
				"`agent` hires a new node, `node_id` from list_nodes reassigns one) keyed by node_id, drop " +
				"assignments by node_id with `remove`" + setupDesc + ". Assignments not named " +
				"here are left exactly as they are. If the current plan already delivered, this instead starts " +
				"a brand-new plan from `assignments` (a fresh plan_id; `remove` no longer applies) - the same " +
				"way create_plan would, and still reusing any node_id from list_nodes. Call with at least one of " +
				changeList + " set - a call that changes nothing is rejected, it is not a valid way to re-read the " +
				"plan (use list_nodes). Errors name the field and the fix: unknown agent, unknown depends_on id, a " +
				"dependency cycle, an empty task, a node currently running, the same node_id twice in one call, a " +
				"`remove` id not in the current plan, " + setupOverride + "hiring an agent whose only deliverable " +
				"this dispatch does not allow, a done plan's edit with no assignments to start a new one, or a " +
				"call with nothing to change. Call after create_plan to correct or extend a plan before execute; " +
				"call list_nodes first to reuse a node instead of hiring a new one.",
		},
		func(tc agent.Context, a editPlanArgs) (planUpsertResult, error) {
			current, err := editPlanPrecheck(tc, c, a.PlanID)
			if err != nil {
				return planUpsertResult{}, err
			}
			if current.Status == "done" {
				return editDeliveredPlan(tc, c, current, nodeID, githubSetup, nodeIsRunning, allowedKinds, onAssignment, agents, a)
			}
			if len(a.Assignments) == 0 && len(a.Remove) == 0 && a.Setup == nil && a.Delivery == nil {
				return planUpsertResult{}, fmt.Errorf("edit_plan: nothing to change - got plan_id %q with no assignments, remove, setup, "+
					"or delivery; edit_plan accepts assignments (each with node_id or agent), remove, setup, and/or delivery - set at least one", a.PlanID)
			}
			return applyEdit(tc, c, current, nodeID, githubSetup, nodeIsRunning, allowedKinds, onAssignment, agents, a)
		},
	)
}

func editPlanPrecheck(tc agent.Context, c *recordstore.Client, planID string) (dag.DagPlanRecord, error) {
	current, ok, err := loadDagPlan(tc, c)
	if err != nil {
		return dag.DagPlanRecord{}, fmt.Errorf("edit_plan: %w", err)
	}
	if !ok {
		return dag.DagPlanRecord{}, fmt.Errorf("edit_plan: no plan exists yet in this chat - call create_plan first")
	}
	if planID != "" && planID != current.PlanID {
		return dag.DagPlanRecord{}, fmt.Errorf("edit_plan: plan_id %q is stale - the current plan is %q", planID, current.PlanID)
	}
	return current, nil
}

// editDeliveredPlan: editing a delivered plan starts a new one from assignments.
func editDeliveredPlan(tc agent.Context, c *recordstore.Client, current dag.DagPlanRecord, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc, agents []string, a editPlanArgs) (planUpsertResult, error) {
	// The delivered-plan wording only fits when there are no assignments to even attempt:
	// a rejection of given assignments must come from newPlanRecord verbatim.
	if len(a.Assignments) == 0 {
		if len(a.Remove) > 0 {
			return planUpsertResult{}, fmt.Errorf("edit_plan: plan %q already delivered - there's nothing left to remove from; drop `remove` and give `assignments` to start a new plan", current.PlanID)
		}
		return planUpsertResult{}, fmt.Errorf("edit_plan: plan %q already delivered - it's finished; give `assignments` to start a new plan for further work", current.PlanID)
	}
	res, err := newPlanRecord(tc, c, nodeID, githubSetup, nodeIsRunning, allowedKinds, onAssignment, agents, a.Assignments, a.Setup, a.Delivery)
	if err != nil {
		return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
	}
	res.Summary = fmt.Sprintf("plan %q had already delivered; started a new plan, %s, from these assignments.\n", current.PlanID, res.PlanID) + res.Summary
	return res, nil
}

func applyEdit(tc agent.Context, c *recordstore.Client, current dag.DagPlanRecord, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc, agents []string, a editPlanArgs) (planUpsertResult, error) {
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
		if err := refuseStopped(tc, a.Assignments); err != nil {
			return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
		}
		upserts, minted, err = upsertNodes(a.Assignments, existingNodes, nodeIsRunning, tc.SessionID(), allowedKinds, agents)
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
	// dag_plan (which validates) saves first, so a rejected call leaves no orphan hired node.
	now := time.Now().UTC()
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
	if _, _, err := c.SaveStructured(tc, "dag_plan", rec, "", lineage); err != nil {
		return planUpsertResult{}, fmt.Errorf("edit_plan: %w", err)
	}
	for _, n := range minted {
		nodeLineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
		if _, _, err := c.SaveStructured(tc, "dag_node", n, n.NodeID, nodeLineage); err != nil {
			return planUpsertResult{}, fmt.Errorf("edit_plan: save dag_node %s: %w; "+
				"the plan was already saved - call edit_plan again to replace it", n.NodeID, err)
		}
	}
	// No dag_plan event here: node cards must not appear (or gain new ones) until
	// execute actually dispatches them.
	return planUpsertResult{
		PlanID: rec.PlanID, Assignments: toAssignmentOutputs(rec.Assignments, nodeAgent),
		Setup: rec.Setup, Delivery: rec.Delivery,
		Summary: summarizePlanRecord(rec, nodeAgent) + setupIgnoredNote(a.Setup, githubSetup),
	}, nil
}

// removeAssignments errors on an unknown id (a typo must not no-op) or a run one, whose result
// seeds later dependents.
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

// mergeAssignments upserts by node_id, keeping current's order and appending new ones.
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
