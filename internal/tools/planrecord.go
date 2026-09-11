// planrecord.go: shared glue between the dag_node/dag_plan records and the
// list_nodes/create_plan/edit_plan/execute tools built on top of them.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

const dagPlanRecordID = "dag_plan:main"

// loadDagPlan reads the chat's current dag_plan record. ok is false when
// this chat has never called create_plan.
func loadDagPlan(ctx context.Context, c *recordstore.Client) (rec dag.DagPlanRecord, revision int, ok bool, err error) {
	raw, rev, ok, err := c.Latest(ctx, dagPlanRecordID)
	if err != nil || !ok {
		return dag.DagPlanRecord{}, 0, ok, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return dag.DagPlanRecord{}, 0, false, fmt.Errorf("dag_plan: stored content doesn't unmarshal: %w", err)
	}
	return rec, rev, true, nil
}

// listDagNodeRecords reads every dag_node record in this chat, sorted by id.
func listDagNodeRecords(ctx context.Context, c *recordstore.Client) ([]dag.DagNodeRecord, error) {
	summaries, err := c.List(ctx, "dag_node")
	if err != nil {
		return nil, fmt.Errorf("list dag_node records: %w", err)
	}
	out := make([]dag.DagNodeRecord, 0, len(summaries))
	for _, s := range summaries {
		// Fail closed on a real read error, unlike !ok (genuinely no record) -
		// silently dropping a live node here strands a later reference to it
		// behind an unrelated "unknown node id" error.
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
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// nodeArtifactIDs returns every artifact id (already "kind:instance") this
// node has authored, from the store's own lineage - c.List's NodeID field is
// exactly "which node wrote this revision", the join list_nodes needs.
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

// firstLine returns s up to its first newline, trimmed - list_nodes' "last
// task" is a glance, not the full assignment text.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// AssignmentMetaFunc optionally stamps assignment.meta.<extension> at plan
// creation/edit - never model-authored. Called once per upserted assignment
// regardless of trigger; key is the supplying extension's own name (empty
// key or empty meta is a no-op for that assignment) - the hook itself
// decides whether it has anything to contribute this dispatch, the same way
// AssignmentFreshnessFunc isn't gated on a trigger either. planID/agentName
// aren't on dag.Assignment itself - passed through so the sdk.Assignment
// conversion at the wiring site (internal/serve) can populate the sdk
// struct fully.
type AssignmentMetaFunc func(ctx agent.Context, planID, agentName string, a dag.Assignment) (key string, meta map[string]any)

// stampAssignmentMeta runs onAssignment over assignments in place - a nil
// hook is a no-op. nodeAgent resolves each assignment's node id to its
// hired agent name (already built by the caller for the response echo).
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

// assignmentInput is one entry of create_plan/edit_plan's `assignments`
// array: either NodeID (reuse an existing node - the caller never invents
// one) or Agent (hire a new one), plus the work itself.
type assignmentInput struct {
	NodeID    string   `json:"node_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
	Checks    []string `json:"checks,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
	Rubric    string   `json:"rubric,omitempty"`
}

// assignmentInputSchema derives T's (createPlanArgs/editPlanArgs) default
// input schema - the same derivation functiontool.New would otherwise do
// implicitly (jsonschema.For) - and constrains assignments[].agent to the
// CURRENT agent roster. A contract the tool itself enforces, not a
// model-specific prompt hint (#slice3 review: a small model omitted `agent`
// in most of its create_plan calls even after the roster was named in the
// rejection text). agent stays optional - node_id is the other valid way to
// fill it in - only its value, when given, is constrained; node_id stays
// free-form (minted ids aren't known ahead of a call).
//
// githubSetup, when non-nil (a trigger-backed dispatch), stops ADVERTISING
// `setup` as useful - a one-line Description saying it's ignored on this
// run - rather than dropping the property outright: jsonschema.For closes
// the object (additionalProperties: false), and ADK's functiontool really
// validates a call against this schema before unmarshalling it, so removing
// `setup` from Properties would make a model that sends it anyway fail
// schema validation - the one outcome this must NOT produce (a real,
// accepted createPlanArgs/editPlanArgs field; setupIgnoredNote in the
// handler is what actually handles it). Widening AdditionalProperties
// instead would silently swallow a genuinely misspelled key (e.g.
// "assignmnets") rather than rejecting it by name, which is worse.
func assignmentInputSchema[T any](githubSetup *dag.Setup) (*jsonschema.Schema, error) {
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
	names := dag.AgentNames()
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

// setupIgnoredNote reports the line to append to create_plan/edit_plan's
// result summary when the model submitted a `setup` the trigger's own
// setup overrides outright (githubSetup != nil) - "" when there's nothing
// to note (a plain chat, or the model omitted setup as the schema now
// asks). The trigger's setup wins unconditionally regardless of repo,
// base_ref, or any other field the model sent - there's nothing left to
// validate, only to say so.
func setupIgnoredNote(submitted, githubSetup *dag.Setup) string {
	if githubSetup == nil || submitted == nil {
		return ""
	}
	return fmt.Sprintf("\nsetup ignored: this run clones %s@%s from the trigger", githubSetup.Repo, githubSetup.BaseRef)
}

// validateAllowedDeliveryKind rejects hiring or reassigning agent when its
// job is coupled to a Delivery.Kind (dag.RequiredDeliveryKind) this dispatch
// doesn't allow - deterministic, at plan-authoring time, so a node whose
// output could never be delivered doesn't burn a full run's tokens only to
// be refused at delivery. allowedKinds empty means unrestricted (a plain
// chat dispatch, no GitHub trigger narrowing it).
func validateAllowedDeliveryKind(agent string, allowedKinds []string) error {
	kind, ok := dag.RequiredDeliveryKind(agent)
	if !ok || len(allowedKinds) == 0 {
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

// describeAssignmentInput renders the fields this assignment actually parsed
// to - a rejection naming node_id/agent as both empty tells the model those
// exact keys are missing, so it isn't left guessing what it sent versus what
// the schema silently dropped (an unrecognized key like `agent_name` never
// reaches this struct at all).
func describeAssignmentInput(in assignmentInput) string {
	return fmt.Sprintf("node_id=%q agent=%q task=%q", in.NodeID, in.Agent, firstLine(in.Task))
}

// assignmentOutput is one assignment in create_plan/edit_plan's response -
// same shape as assignmentInput but always carries the resolved node_id and
// agent (minted ids the model must see to depend on or hire the node again).
type assignmentOutput struct {
	NodeID    string   `json:"node_id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// planUpsertResult is create_plan/edit_plan's shared response shape.
type planUpsertResult struct {
	PlanID      string             `json:"plan_id"`
	Assignments []assignmentOutput `json:"assignments"`
	Setup       *dag.Setup         `json:"setup,omitempty"`
	Delivery    *dag.Delivery      `json:"delivery,omitempty"`
	Summary     string             `json:"summary"`
}

// resolveNodeIndex reports whether ref is a decimal index into a
// same-call assignments array (e.g. "0") - the convention that lets one
// create_plan/edit_plan call build a multi-node DAG in one shot despite ids
// being system-minted: a `depends_on` entry can't name a sibling's node_id
// before it exists, so it names the sibling's position instead.
func resolveNodeIndex(ref string, n int) (int, bool) {
	i, err := strconv.Atoi(ref)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// upsertNodes applies create_plan/edit_plan's shared assignment-resolution
// logic: mint a dag_node for every entry with no NodeID, resolve
// depends_on's same-call positional references, and reject a reference to a
// node currently live (executing) in this run. existingNodes is every
// dag_node record already in this chat (for reuse and id-minting
// uniqueness); nodeIsLive is nil-safe (no executor wired = never live, e.g.
// a bound/test config). chatID seeds a minted node's A2A ContextID
// (quackagent.WorkerSessionID) - the same deterministic id the native A2A
// path (internal/serve buildAgents) independently recomputes when it
// actually dispatches to this node, so the stored value is never a guess.
func upsertNodes(inputs []assignmentInput, existingNodes []dag.DagNodeRecord, nodeIsLive func(nodeID string) bool, chatID string, allowedKinds []string) ([]dag.Assignment, []dag.DagNodeRecord, error) {
	known := make(map[string]dag.DagNodeRecord, len(existingNodes))
	var existingIDs []string
	for _, n := range existingNodes {
		known[n.NodeID] = n
		existingIDs = append(existingIDs, n.NodeID)
	}

	nodeIDs := make([]string, len(inputs))
	var minted []dag.DagNodeRecord
	for i, in := range inputs {
		if err := dag.ValidateWorkdir(in.Workdir); err != nil {
			return nil, nil, fmt.Errorf("assignments[%d].%w", i, err)
		}
		switch {
		case in.NodeID != "":
			n, ok := known[in.NodeID]
			if !ok {
				return nil, nil, fmt.Errorf("assignments[%d].node_id: unknown node id %q - list_nodes shows every node already hired", i, in.NodeID)
			}
			if nodeIsLive != nil && nodeIsLive(in.NodeID) {
				return nil, nil, fmt.Errorf("assignments[%d].node_id: %q is currently running - wait for it to finish before reassigning it", i, in.NodeID)
			}
			if err := validateAllowedDeliveryKind(n.Agent, allowedKinds); err != nil {
				return nil, nil, fmt.Errorf("assignments[%d].%w", i, err)
			}
			nodeIDs[i] = n.NodeID
		case in.Agent != "":
			if err := dag.ValidateAgentName(in.Agent); err != nil {
				return nil, nil, fmt.Errorf("assignments[%d].%w", i, err)
			}
			if err := validateAllowedDeliveryKind(in.Agent, allowedKinds); err != nil {
				return nil, nil, fmt.Errorf("assignments[%d].%w", i, err)
			}
			id := dag.MintNodeID(in.Agent, existingIDs)
			existingIDs = append(existingIDs, id)
			rec := dag.DagNodeRecord{NodeID: id, Agent: in.Agent, Status: dag.StatusQueued, ContextID: quackagent.WorkerSessionID(chatID, id)}
			known[id] = rec
			minted = append(minted, rec)
			nodeIDs[i] = id
		default:
			return nil, nil, fmt.Errorf("assignments[%d]: has neither node_id nor agent set (%s) - give node_id "+
				"(an existing node from list_nodes) or agent (one of: %s)", i, describeAssignmentInput(in), strings.Join(dag.AgentNames(), ", "))
		}
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
		dependsOn := make([]string, len(in.DependsOn))
		for j, dep := range in.DependsOn {
			if idx, ok := resolveNodeIndex(dep, len(inputs)); ok {
				dependsOn[j] = nodeIDs[idx]
				continue
			}
			if _, ok := known[dep]; !ok {
				return nil, nil, fmt.Errorf("assignments[%d].depends_on: unknown node id %q - name an existing node id or this call's own assignment index", i, dep)
			}
			dependsOn[j] = dep
		}
		assignments[i] = dag.Assignment{
			NodeID: nodeIDs[i], Task: in.Task, DependsOn: dependsOn,
			Checks: in.Checks, Workdir: in.Workdir, Rubric: in.Rubric,
		}
	}
	return assignments, minted, nil
}

// summarizePlanRecord renders rec for the model's own review before it calls
// execute - never shown to the user.
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
