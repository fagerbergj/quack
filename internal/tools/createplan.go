package tools

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

type createPlanArgs struct {
	Assignments []assignmentInput `json:"assignments"`
	Setup       *dag.Setup        `json:"setup,omitempty"`
	Delivery    *dag.Delivery     `json:"delivery,omitempty"`
}

// NewCreatePlanTool: a non-nil githubSetup always overrides Setup, since the trigger knows the real repo.
// nodeID is the authoring lineage id (e.g. "orchestrator"), unrelated to a dag_node's node_id.
func NewCreatePlanTool(c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc, agents []string) (tool.Tool, error) {
	artifactDesc := "`assignments[].checks` are OPTIONAL - you have NOT seen the repo yet, so do NOT guess its " +
		"commands: the trust gate derives a code node's checks from the repo itself after the node clones it."
	schema, err := assignmentInputSchema[createPlanArgs](githubSetup, agents)
	if err != nil {
		return nil, fmt.Errorf("create_plan: %w", err)
	}
	setupDesc := " Every plan whose deliverable touches GitHub declares `setup` " +
		"({repo, base_ref, work_branch} - the branch the work happens on) and `delivery` ({kind: " +
		"\"pull_request\"|\"review\"|\"comment\"}); the harness runs both AFTER the trust gate, deterministically " +
		"- a node never pushes, opens a PR, or posts a review itself. Put the real repo/PR in a node's `task` " +
		"text, never a guessed one."
	if githubSetup != nil {
		setupDesc = fmt.Sprintf(" This dispatch already came with its own repo/base_ref (a GitHub trigger) - "+
			"this run clones %s@%s regardless, so `setup` isn't offered here; declare `delivery` ({kind: "+
			"\"pull_request\"|\"review\"|\"comment\"}) if the deliverable touches GitHub. Put the real PR/issue "+
			"context in a node's `task` text.", githubSetup.Repo, githubSetup.BaseRef)
	}
	return functiontool.New[createPlanArgs, planUpsertResult](
		functiontool.Config{
			Name:        "create_plan",
			InputSchema: schema,
			Description: "Tool to start a plan: a list of assignments tying nodes (people doing an agent's job) " +
				"to work. Each assignment is EITHER `agent` (a name from the Agents list - hires a new node for " +
				"that job) OR `node_id` (from list_nodes - reassigns a node already hired), plus `task` (self-" +
				"contained - the node sees only this text) and `depends_on` (the tasks this one references, a " +
				"HANDOFF not a data dependency: this assignment runs after the named ids and receives their " +
				"result, nothing else - either a real node_id or, for a sibling being hired in this SAME call, " +
				"that sibling's 0-based position in this `assignments` array, since a brand-new node has no id yet). " +
				artifactDesc + setupDesc + " Hiring an agent whose only deliverable this dispatch does not allow " +
				"(e.g. a `code-reviewer` when only a pull request can be delivered) is rejected. Returns the plan " +
				"(assignments with minted node_ids) - call edit_plan to change it, or execute to run it. Do NOT " +
				"call for tasks you can answer directly.",
		},
		func(tc agent.Context, a createPlanArgs) (planUpsertResult, error) {
			if len(a.Assignments) == 0 {
				return planUpsertResult{}, fmt.Errorf("create_plan: assignments must be non-empty")
			}
			res, err := newPlanRecord(tc, c, nodeID, githubSetup, nodeIsRunning, allowedKinds, onAssignment, agents, a.Assignments, a.Setup, a.Delivery)
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("create_plan: %w", err)
			}
			return res, nil
		},
	)
}

// newPlanRecord: create_plan, and edit_plan on a delivered plan. submittedSetup is the raw arg, so
// setupIgnoredNote reports what the model actually sent.
func newPlanRecord(tc agent.Context, c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool, allowedKinds []string, onAssignment AssignmentMetaFunc, agents []string, inputs []assignmentInput, submittedSetup *dag.Setup, delivery *dag.Delivery) (planUpsertResult, error) {
	existing, err := listDagNodeRecords(tc, c)
	if err != nil {
		return planUpsertResult{}, err
	}
	if err := refuseStopped(tc, inputs); err != nil {
		return planUpsertResult{}, err
	}
	assignments, minted, err := upsertNodes(inputs, existing, nodeIsRunning, tc.SessionID(), allowedKinds, agents)
	if err != nil {
		return planUpsertResult{}, err
	}
	nodeAgent := map[string]string{}
	for _, n := range existing {
		nodeAgent[n.NodeID] = n.Agent
	}
	for _, n := range minted {
		nodeAgent[n.NodeID] = n.Agent
	}

	planID := uuid.NewString()
	stampAssignmentMeta(tc, planID, nodeAgent, assignments, onAssignment)
	setup := submittedSetup
	if githubSetup != nil {
		s := *githubSetup
		setup = &s
	}
	rec := dag.DagPlanRecord{PlanID: planID, Assignments: assignments, Setup: setup, Delivery: delivery, Status: "planned"}

	// dag_plan (which validates) saves first, so a rejected call leaves no orphan hired node.
	now := time.Now().UTC()
	lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
	if _, _, err := c.SaveStructured(tc, "dag_plan", rec, "", lineage); err != nil {
		return planUpsertResult{}, err
	}
	for _, n := range minted {
		nodeLineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
		if _, _, err := c.SaveStructured(tc, "dag_node", n, n.NodeID, nodeLineage); err != nil {
			return planUpsertResult{}, fmt.Errorf("save dag_node %s: %w; the plan was already saved - call again to replace it", n.NodeID, err)
		}
	}

	// No dag_plan event: node cards must not appear before execute runs the draft.
	return planUpsertResult{
		PlanID: rec.PlanID, Assignments: toAssignmentOutputs(rec.Assignments, nodeAgent),
		Setup: rec.Setup, Delivery: rec.Delivery,
		Summary: summarizePlanRecord(rec, nodeAgent) + setupIgnoredNote(submittedSetup, githubSetup),
	}, nil
}
