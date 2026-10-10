package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

const dagPlanRecordID = "dag_plan:main"

// loadDagPlan: ok is false when this chat has never called create_plan.
func loadDagPlan(ctx context.Context, c *recordstore.Client) (rec dag.DagPlanRecord, ok bool, err error) {
	raw, _, ok, err := c.Latest(ctx, dagPlanRecordID)
	if err != nil || !ok {
		return dag.DagPlanRecord{}, ok, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return dag.DagPlanRecord{}, false, fmt.Errorf("dag_plan: stored content doesn't unmarshal: %w", err)
	}
	return rec, true, nil
}

func listDagNodeRecords(ctx context.Context, c *recordstore.Client) ([]dag.DagNodeRecord, error) {
	summaries, err := c.List(ctx, "dag_node")
	if err != nil {
		return nil, fmt.Errorf("list dag_node records: %w", err)
	}
	out := make([]dag.DagNodeRecord, 0, len(summaries))
	for _, s := range summaries {
		// Fail closed on a read error: dropping a live node strands later references behind "unknown node id".
		raw, _, ok, err := c.Latest(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("list dag_node records: read %s: %w", s.ID, err)
		}
		if !ok {
			continue
		}
		var rec dag.DagNodeRecord
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b dag.DagNodeRecord) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out, nil
}

// nodeArtifactIDs: ids this node authored, from c.List's NodeID (the node that wrote each revision).
func nodeArtifactIDs(ctx context.Context, c *recordstore.Client, nodeID string) ([]string, error) {
	all, err := c.List(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range all {
		if a.NodeID != nodeID || a.Kind == "dag_node" || a.Kind == "dag_plan" {
			continue
		}
		out = append(out, a.ID)
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// AssignmentMetaFunc stamps assignment.meta.<key> (key = the extension's name) once per upserted assignment,
// never model-authored; an empty key or meta is a no-op.
type AssignmentMetaFunc func(ctx agent.Context, planID, agentName string, a dag.Assignment) (key string, meta map[string]any)

// stampAssignmentMeta runs onAssignment over assignments in place; nil is a no-op.
func stampAssignmentMeta(tc agent.Context, planID string, nodeAgent map[string]string, assignments []dag.Assignment, onAssignment AssignmentMetaFunc) {
	if onAssignment == nil {
		return
	}
	for i := range assignments {
		key, m := onAssignment(tc, planID, nodeAgent[assignments[i].NodeID], assignments[i])
		if key == "" || len(m) == 0 {
			continue
		}
		if assignments[i].Meta == nil {
			assignments[i].Meta = map[string]map[string]any{}
		}
		assignments[i].Meta[key] = m
	}
}

// errStoppedNode is what the model gets for re-running a node the user stopped: the stop is
// the user's decision for this turn, so only an explicit user retry may undo it.
func errStoppedNode(nodeID string) error {
	return fmt.Errorf("node %q was stopped by the user this turn - do not re-run, reassign or restate it; leave it stopped and carry on with other work, or ask the user", nodeID)
}

func refuseStopped(ctx context.Context, inputs []assignmentInput) error {
	stopped := NodeStoppedFromContext(ctx)
	for _, in := range inputs {
		if in.NodeID != "" && stopped(in.NodeID) {
			return errStoppedNode(in.NodeID)
		}
	}
	return nil
}

// assignmentInput: NodeID reuses an existing node, Agent hires a new one.
type assignmentInput struct {
	NodeID    string   `json:"node_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
	Checks    []string `json:"checks,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
	Rubric    string   `json:"rubric,omitempty"`
}

// assignmentInputSchema constrains assignments[].agent to the current roster. With githubSetup it only
// re-describes `setup` as ignored: the schema is closed, so dropping the property would fail validation.
func assignmentInputSchema[T any](githubSetup *dag.Setup, names []string) (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, fmt.Errorf("derive input schema: %w", err)
	}
	assignments, ok := schema.Properties["assignments"]
	if !ok || assignments.Items == nil {
		return nil, fmt.Errorf("derive input schema: assignments[].items missing")
	}
	agentProp, ok := assignments.Items.Properties["agent"]
	if !ok {
		return nil, fmt.Errorf("derive input schema: assignments[].agent missing")
	}
	agentProp.Enum = make([]any, len(names))
	for i, n := range names {
		agentProp.Enum[i] = n
	}
	if githubSetup != nil {
		if setupProp, ok := schema.Properties["setup"]; ok {
			setupProp.Description = fmt.Sprintf("Ignored on this run: the trigger fixes repo/base_ref to %s@%s.", githubSetup.Repo, githubSetup.BaseRef)
		}
	}
	return schema, nil
}

// setupIgnoredNote: the summary line when the trigger's setup overrides a submitted one; "" otherwise.
func setupIgnoredNote(submitted, githubSetup *dag.Setup) string {
	if githubSetup == nil || submitted == nil {
		return ""
	}
	return fmt.Sprintf("\nsetup ignored: this run clones %s@%s from the trigger", githubSetup.Repo, githubSetup.BaseRef)
}

// validateAllowedDeliveryKind refuses, at plan time, an agent whose only delivery kind the grant
// excludes. nil = unrestricted; non-nil empty denies all, like the gate's partitionByAllowedKinds.
func validateAllowedDeliveryKind(agent string, allowedKinds []string) error {
	kind, ok := dag.RequiredDeliveryKind(agent)
	if !ok || allowedKinds == nil {
		return nil
	}
	for _, k := range allowedKinds {
		if k == kind {
			return nil
		}
	}
	return fmt.Errorf("agent: %q only delivers as %q, which this dispatch does not allow (allowed: %s)",
		agent, kind, strings.Join(allowedKinds, ", "))
}

// describeAssignmentInput renders the parsed fields, so a rejection shows which keys never arrived.
func describeAssignmentInput(in assignmentInput) string {
	return fmt.Sprintf("node_id=%q agent=%q task=%q", in.NodeID, in.Agent, firstLine(in.Task))
}

// assignmentOutput always carries the resolved node_id and agent: the model needs minted ids.
type assignmentOutput struct {
	NodeID    string   `json:"node_id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type planUpsertResult struct {
	PlanID      string             `json:"plan_id"`
	Assignments []assignmentOutput `json:"assignments"`
	Setup       *dag.Setup         `json:"setup,omitempty"`
	Delivery    *dag.Delivery      `json:"delivery,omitempty"`
	Summary     string             `json:"summary"`
}

// resolveNodeIndex: a depends_on entry may name a same-call sibling by index, since its id isn't minted yet.
func resolveNodeIndex(ref string, n int) (int, bool) {
	i, err := strconv.Atoi(ref)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// upsertNodes mints nodes, resolves positional depends_on and refuses live nodes; nodeIsLive may be nil.
// A minted ContextID is WorkerSessionID(chatID, id), the same id dispatch recomputes.
func upsertNodes(inputs []assignmentInput, existingNodes []dag.DagNodeRecord, nodeIsLive func(nodeID string) bool, chatID string, allowedKinds, agents []string) ([]dag.Assignment, []dag.DagNodeRecord, error) {
	known := make(map[string]dag.DagNodeRecord, len(existingNodes))
	var existingIDs []string
	for _, n := range existingNodes {
		known[n.NodeID] = n
		existingIDs = append(existingIDs, n.NodeID)
	}

	nodeIDs := make([]string, len(inputs))
	st := upsertState{known: known, existingIDs: existingIDs, nodeIsLive: nodeIsLive, chatID: chatID, allowedKinds: allowedKinds, agents: agents}
	for i, in := range inputs {
		id, err := st.resolveOne(i, in)
		if err != nil {
			return nil, nil, err
		}
		nodeIDs[i] = id
	}

	seenNodeID := make(map[string]int, len(nodeIDs))
	for i, id := range nodeIDs {
		if j, dup := seenNodeID[id]; dup {
			return nil, nil, fmt.Errorf("assignments[%d].node_id: %q is already assignments[%d] in this call - one assignment per node per plan", i, id, j)
		}
		seenNodeID[id] = i
	}

	assignments := make([]dag.Assignment, len(inputs))
	for i, in := range inputs {
		dependsOn, err := st.resolveDependsOn(i, in.DependsOn, nodeIDs)
		if err != nil {
			return nil, nil, err
		}
		assignments[i] = dag.Assignment{
			NodeID: nodeIDs[i], Task: in.Task, DependsOn: dependsOn,
			Checks: in.Checks, Workdir: in.Workdir, Rubric: in.Rubric,
		}
	}
	return assignments, st.minted, nil
}

type upsertState struct {
	known        map[string]dag.DagNodeRecord
	existingIDs  []string
	minted       []dag.DagNodeRecord
	nodeIsLive   func(nodeID string) bool
	chatID       string
	allowedKinds []string
	agents       []string // the run's roster (dag.AgentNamesFor), not a newer reload's
}

// resolveOne validates workdir first, so a bad dir fails on either branch.
func (st *upsertState) resolveOne(i int, in assignmentInput) (string, error) {
	if err := dag.ValidateWorkdir(in.Workdir); err != nil {
		return "", fmt.Errorf("assignments[%d].%w", i, err)
	}
	switch {
	case in.NodeID != "":
		n, ok := st.known[in.NodeID]
		if !ok {
			return "", fmt.Errorf("assignments[%d].node_id: unknown node id %q - list_nodes shows every node already hired", i, in.NodeID)
		}
		if st.nodeIsLive != nil && st.nodeIsLive(in.NodeID) {
			return "", fmt.Errorf("assignments[%d].node_id: %q is currently running - wait for it to finish before reassigning it", i, in.NodeID)
		}
		// A reload may have dropped this node's agent from the roster this run dispatches on.
		if err := dag.ValidateAgentNameIn(n.Agent, st.agents); err != nil {
			return "", fmt.Errorf("assignments[%d].node_id: %q's %w", i, in.NodeID, err)
		}
		if err := validateAllowedDeliveryKind(n.Agent, st.allowedKinds); err != nil {
			return "", fmt.Errorf("assignments[%d].%w", i, err)
		}
		return n.NodeID, nil
	case in.Agent != "":
		if err := dag.ValidateAgentNameIn(in.Agent, st.agents); err != nil {
			return "", fmt.Errorf("assignments[%d].%w", i, err)
		}
		if err := validateAllowedDeliveryKind(in.Agent, st.allowedKinds); err != nil {
			return "", fmt.Errorf("assignments[%d].%w", i, err)
		}
		id := dag.MintNodeID(in.Agent, st.existingIDs)
		st.existingIDs = append(st.existingIDs, id)
		rec := dag.DagNodeRecord{NodeID: id, Agent: in.Agent, Status: dag.StatusQueued, ContextID: quackagent.WorkerSessionID(st.chatID, id)}
		st.known[id] = rec
		st.minted = append(st.minted, rec)
		return id, nil
	default:
		return "", fmt.Errorf("assignments[%d]: has neither node_id nor agent set (%s) - give node_id "+
			"(an existing node from list_nodes) or agent (one of: %s)", i, describeAssignmentInput(in), strings.Join(st.agents, ", "))
	}
}

func (st *upsertState) resolveDependsOn(i int, deps []string, nodeIDs []string) ([]string, error) {
	out := make([]string, len(deps))
	for j, dep := range deps {
		if idx, ok := resolveNodeIndex(dep, len(nodeIDs)); ok {
			out[j] = nodeIDs[idx]
			continue
		}
		if _, ok := st.known[dep]; !ok {
			return nil, fmt.Errorf("assignments[%d].depends_on: unknown node id %q - name an existing node id or this call's own assignment index", i, dep)
		}
		out[j] = dep
	}
	return out, nil
}

// summarizePlanRecord is for the model's review before execute, never shown to the user.
func summarizePlanRecord(rec dag.DagPlanRecord, nodeAgent map[string]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan %s (%d assignment(s)) - review before executing:", rec.PlanID, len(rec.Assignments))
	for _, a := range rec.Assignments {
		fmt.Fprintf(&sb, "\n- %s (%s)", a.NodeID, nodeAgent[a.NodeID])
		if len(a.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(a.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(a.Task))
	}
	if rec.Setup != nil {
		fmt.Fprintf(&sb, "\nsetup: repo=%q base_ref=%q work_branch=%q", rec.Setup.Repo, rec.Setup.BaseRef, rec.Setup.WorkBranch)
	}
	if rec.Delivery != nil {
		fmt.Fprintf(&sb, "\ndelivery: kind=%q", rec.Delivery.Kind)
	}
	return sb.String()
}

func toAssignmentOutputs(assignments []dag.Assignment, nodeAgent map[string]string) []assignmentOutput {
	out := make([]assignmentOutput, len(assignments))
	for i, a := range assignments {
		out[i] = assignmentOutput{NodeID: a.NodeID, Agent: nodeAgent[a.NodeID], Task: a.Task, DependsOn: a.DependsOn}
	}
	return out
}
