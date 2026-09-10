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
	"github.com/fagerbergj/quack/internal/stream"
)

type createPlanArgs struct {
	Assignments []assignmentInput `json:"assignments"`
	Setup       *dag.Setup        `json:"setup,omitempty"`
	Delivery    *dag.Delivery     `json:"delivery,omitempty"`
}

// NewCreatePlanTool: starts a fresh plan tying assignments to nodes - hiring
// a node (agent) or reassigning an existing one (node_id, from list_nodes)
// per assignment. githubSetup, when non-nil, always overrides Setup: the
// GitHub extension already knows the real repo/branch/PR, so the model's
// guess never wins. Saves the same dag_plan/dag_node records edit_plan and
// the artifact panel read.
func NewCreatePlanTool(c *recordstore.Client, nodeID string, githubSetup *dag.Setup, nodeIsRunning func(string) bool) (tool.Tool, error) {
	artifactDesc := "`assignments[].checks` are OPTIONAL - you have NOT seen the repo yet, so do NOT guess its " +
		"commands: the trust gate derives a code node's checks from the repo itself after the node clones it."
	return functiontool.New[createPlanArgs, planUpsertResult](
		functiontool.Config{
			Name: "create_plan",
			Description: "Tool to start a plan: a list of assignments tying nodes (people doing an agent's job) " +
				"to work. Each assignment is EITHER `agent` (a name from the Agents list - hires a new node for " +
				"that job) OR `node_id` (from list_nodes - reassigns a node already hired), plus `task` (self-" +
				"contained - the node sees only this text) and `depends_on` (the tasks this one references, a " +
				"HANDOFF not a data dependency: this assignment runs after the named ids and receives their " +
				"result, nothing else - either a real node_id or, for a sibling being hired in this SAME call, " +
				"that sibling's 0-based position in this `assignments` array, since a brand-new node has no id yet). " +
				artifactDesc + " Every plan whose deliverable touches GitHub declares `setup` " +
				"({repo, base_ref, work_branch} - the branch the work happens on) and `delivery` ({kind: " +
				"\"pull_request\"|\"review\"|\"comment\"}); the harness runs both AFTER the trust gate, deterministically " +
				"- a node never pushes, opens a PR, or posts a review itself. Returns the plan (assignments with " +
				"minted node_ids) - call edit_plan to change it, or execute to run it. Do NOT call for tasks you " +
				"can answer directly.",
		},
		func(tc agent.Context, a createPlanArgs) (planUpsertResult, error) {
			if len(a.Assignments) == 0 {
				return planUpsertResult{}, fmt.Errorf("create_plan: assignments must be non-empty")
			}
			existing, err := listDagNodeRecords(tc, c)
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("create_plan: %w", err)
			}
			assignments, minted, err := upsertNodes(a.Assignments, existing, nodeIsRunning, tc.SessionID())
			if err != nil {
				return planUpsertResult{}, fmt.Errorf("create_plan: %w", err)
			}
			setup := a.Setup
			if githubSetup != nil {
				s := *githubSetup
				setup = &s
			}
			rec := dag.DagPlanRecord{PlanID: uuid.NewString(), Assignments: assignments, Setup: setup, Delivery: a.Delivery, Status: "planned"}

			nodeAgent := map[string]string{}
			for _, n := range existing {
				nodeAgent[n.NodeID] = n.Agent
			}
			for _, n := range minted {
				nodeAgent[n.NodeID] = n.Agent
			}

			now := time.Now().UTC()
			for _, n := range minted {
				lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
				if _, _, err := c.SaveStructured(tc, "dag_node", n, n.NodeID, lineage); err != nil {
					return planUpsertResult{}, fmt.Errorf("create_plan: save dag_node %s: %w", n.NodeID, err)
				}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Author: "worker", SavedAt: now}
			if _, _, err := c.SaveStructured(tc, "dag_plan", rec, "", lineage); err != nil {
				return planUpsertResult{}, fmt.Errorf("create_plan: %w", err)
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
