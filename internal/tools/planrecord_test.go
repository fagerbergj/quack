package tools

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestUpsertNodesUnknownAgentListsRoster covers the contract table's "unknown
// agent" row: the error names the field and lists every valid agent.
func TestUpsertNodesUnknownAgentListsRoster(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	_, _, err := upsertNodes([]assignmentInput{{Agent: "not-a-real-agent", Task: "x"}}, nil, nil, "chat1", nil)
	if err == nil {
		t.Fatal("want an error for an unknown agent")
	}
	for _, want := range []string{"assignments[0]", "unknown agent", "web-researcher", "code-implementer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestUpsertNodesUnknownNodeID covers reassigning a node_id list_nodes never showed.
func TestUpsertNodesUnknownNodeID(t *testing.T) {
	_, _, err := upsertNodes([]assignmentInput{{NodeID: "ghost-1", Task: "x"}}, nil, nil, "chat1", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown node id") {
		t.Errorf("err = %v, want an unknown node id error", err)
	}
}

// TestUpsertNodesNodeCurrentlyRunning covers reassigning a live node.
func TestUpsertNodesNodeCurrentlyRunning(t *testing.T) {
	existing := []dag.DagNodeRecord{{NodeID: "impl-1", Agent: "code-implementer"}}
	running := func(id string) bool { return id == "impl-1" }
	_, _, err := upsertNodes([]assignmentInput{{NodeID: "impl-1", Task: "x"}}, existing, running, "chat1", nil)
	if err == nil || !strings.Contains(err.Error(), "currently running") {
		t.Errorf("err = %v, want a currently-running error", err)
	}
}

// TestUpsertNodesUnknownDependsOn covers depends_on naming neither an
// existing node id nor a same-call positional index.
func TestUpsertNodesUnknownDependsOn(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	_, _, err := upsertNodes([]assignmentInput{
		{Agent: "web-researcher", Task: "x", DependsOn: []string{"ghost"}},
	}, nil, nil, "chat1", nil)
	if err == nil || !strings.Contains(err.Error(), "depends_on") {
		t.Errorf("err = %v, want a depends_on error", err)
	}
}

// TestUpsertNodesDuplicateNodeIDInOneCall covers the same node_id appearing
// twice in one create_plan/edit_plan call - a real error, not a silent
// last-write-wins collapse (mergeAssignments would otherwise swallow it).
func TestUpsertNodesDuplicateNodeIDInOneCall(t *testing.T) {
	existing := []dag.DagNodeRecord{{NodeID: "impl-1", Agent: "code-implementer"}}
	_, _, err := upsertNodes([]assignmentInput{
		{NodeID: "impl-1", Task: "first"},
		{NodeID: "impl-1", Task: "second"},
	}, existing, nil, "chat1", nil)
	if err == nil {
		t.Fatal("want an error for the same node_id twice in one call")
	}
	if !strings.Contains(err.Error(), "impl-1") || !strings.Contains(err.Error(), "assignments[1]") {
		t.Errorf("err = %v, want it to name assignments[1] and impl-1", err)
	}
}

// TestUpsertNodesPositionalDependsOn covers a sibling being hired in the same
// call referenced by its 0-based position, since it has no node_id yet.
func TestUpsertNodesPositionalDependsOn(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil, nil)
	assignments, minted, err := upsertNodes([]assignmentInput{
		{Agent: "web-researcher", Task: "research"},
		{Agent: "synthesizer", Task: "write it up", DependsOn: []string{"0"}},
	}, nil, nil, "chat1", nil)
	if err != nil {
		t.Fatalf("upsertNodes: %v", err)
	}
	if len(minted) != 2 {
		t.Fatalf("minted = %d nodes, want 2", len(minted))
	}
	if got, want := assignments[1].DependsOn, []string{assignments[0].NodeID}; got[0] != want[0] {
		t.Errorf("assignments[1].DependsOn = %v, want [%q] (the minted sibling id)", got, want[0])
	}
}

// TestUpsertNodesMintedIDEcho covers the minted-id echo: a hired node's
// assignment carries the freshly minted, agent-prefixed id back to the caller.
func TestUpsertNodesMintedIDEcho(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	assignments, minted, err := upsertNodes([]assignmentInput{{Agent: "web-researcher", Task: "x"}}, nil, nil, "chat1", nil)
	if err != nil {
		t.Fatalf("upsertNodes: %v", err)
	}
	if len(minted) != 1 || minted[0].NodeID != "web-researcher-1" {
		t.Fatalf("minted = %+v, want one node web-researcher-1", minted)
	}
	if assignments[0].NodeID != "web-researcher-1" {
		t.Errorf("assignments[0].NodeID = %q, want the minted id web-researcher-1", assignments[0].NodeID)
	}
}

// TestRemoveAssignmentsUnknownIDErrors covers edit_plan's `remove`: a typo'd
// node_id must error, not silently no-op.
func TestRemoveAssignmentsUnknownIDErrors(t *testing.T) {
	current := []dag.Assignment{{NodeID: "impl-1", Task: "x"}}
	if _, err := removeAssignments(current, []string{"ghost"}); err == nil {
		t.Fatal("want an error removing a node_id not on the plan")
	}
	out, err := removeAssignments(current, []string{"impl-1"})
	if err != nil {
		t.Fatalf("removeAssignments: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want the known node_id actually removed", out)
	}
}

// TestMergeAssignmentsUpsertsExistingAndAppendsNew covers edit_plan's core
// merge: an upsert matching a current node_id replaces it in place, and a
// brand-new node_id is appended.
func TestMergeAssignmentsUpsertsExistingAndAppendsNew(t *testing.T) {
	current := []dag.Assignment{{NodeID: "impl-1", Task: "old task"}, {NodeID: "rev-1", Task: "review"}}
	upserts := []dag.Assignment{{NodeID: "impl-1", Task: "new task"}, {NodeID: "web-researcher-1", Task: "research"}}
	out := mergeAssignments(current, upserts)
	if len(out) != 3 {
		t.Fatalf("out = %+v, want 3 assignments (2 current with one replaced, 1 new)", out)
	}
	if out[0].NodeID != "impl-1" || out[0].Task != "new task" {
		t.Errorf("out[0] = %+v, want impl-1 updated in place with the new task", out[0])
	}
	if out[1].NodeID != "rev-1" || out[1].Task != "review" {
		t.Errorf("out[1] = %+v, want rev-1 untouched", out[1])
	}
	if out[2].NodeID != "web-researcher-1" {
		t.Errorf("out[2] = %+v, want the new node appended", out[2])
	}
}

// TestBuildNodeSummariesEmptyChat covers list_nodes against a chat that has
// never called create_plan.
func TestBuildNodeSummariesEmptyChat(t *testing.T) {
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	out, err := buildNodeSummaries(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("buildNodeSummaries: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want no nodes for an empty chat", out)
	}
}

// TestBuildNodeSummariesReportsTerminalStatusAndContextID is the BLOCKING
// regression test: list_nodes must show a node's status as it actually is
// after it finishes running, not stuck at "queued" forever once it's no
// longer live - and must surface the node's A2A context_id.
func TestBuildNodeSummariesReportsTerminalStatusAndContextID(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	ctx := context.Background()
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, "quack", "u1", "chat1")
	rec := dag.DagNodeRecord{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusQueued, ContextID: "chat1:impl-1"}
	if _, _, err := c.SaveStructured(ctx, "dag_node", rec, rec.NodeID, recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}

	// Not live: list_nodes must read the terminal status off the record itself.
	notRunning := func(string) bool { return false }
	before, err := buildNodeSummaries(ctx, c, notRunning)
	if err != nil {
		t.Fatalf("buildNodeSummaries: %v", err)
	}
	if len(before) != 1 || before[0].Status != "queued" {
		t.Fatalf("before = %+v, want status queued (setup for this test)", before)
	}

	// Realistic transition sequence: queued -> running -> done. CanTransition
	// rejects a bare queued -> done jump.
	if err := dag.UpdateDagNodeStatus(ctx, svc, "quack", "u1", "chat1", "impl-1", dag.StatusRunning); err != nil {
		t.Fatalf("UpdateDagNodeStatus (running): %v", err)
	}
	if err := dag.UpdateDagNodeStatus(ctx, svc, "quack", "u1", "chat1", "impl-1", dag.StatusDone); err != nil {
		t.Fatalf("UpdateDagNodeStatus (done): %v", err)
	}

	after, err := buildNodeSummaries(ctx, c, notRunning)
	if err != nil {
		t.Fatalf("buildNodeSummaries: %v", err)
	}
	if len(after) != 1 || after[0].Status != "done" {
		t.Errorf("after = %+v, want status done - list_nodes must not stay stuck at queued once the node has finished", after)
	}
	if after[0].ContextID != "chat1:impl-1" {
		t.Errorf("after[0].ContextID = %q, want the node's A2A context_id", after[0].ContextID)
	}
}

// TestUpsertNodesNeitherFieldSetEchoesReceivedShape is the QA rig regression
// test: the 9B kept sending an assignment with neither node_id nor agent
// set, and the old error gave it nothing to correct from. The new one must
// name what actually parsed (both empty) and the roster to pick from.
func TestUpsertNodesNeitherFieldSetEchoesReceivedShape(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}, {Name: "code-reviewer"}}, nil, nil)
	_, _, err := upsertNodes([]assignmentInput{{Task: "do the thing"}}, nil, nil, "chat1", nil)
	if err == nil {
		t.Fatal("want an error when neither node_id nor agent is set")
	}
	for _, want := range []string{"node_id=\"\"", "agent=\"\"", "do the thing", "code-implementer", "code-reviewer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestUpsertNodesRejectsAgentWhoseDeliveryIsNotAllowed is the QA rig
// regression test: an implement-only dispatch (allowedKinds=["pull_request"])
// hired a code-reviewer node anyway, which ran ~90k tokens before delivery
// itself refused it ("delivery kind review not in allowed set"). The
// rejection must happen at plan-authoring time, before any node runs.
func TestUpsertNodesRejectsAgentWhoseDeliveryIsNotAllowed(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	_, _, err := upsertNodes([]assignmentInput{{Agent: "code-reviewer", Task: "review it"}}, nil, nil, "chat1", []string{"pull_request"})
	if err == nil {
		t.Fatal("want an error hiring code-reviewer when only pull_request delivery is allowed")
	}
	for _, want := range []string{"code-reviewer", "review", "pull_request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestUpsertNodesAllowsAgentWhenDeliveryUnrestricted covers the non-
// restrictive cases: no allowedKinds (a plain chat dispatch) and an agent
// with no delivery coupling (e.g. web-researcher) both pass unconditionally.
func TestUpsertNodesAllowsAgentWhenDeliveryUnrestricted(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}, {Name: "web-researcher"}}, nil, nil)
	if _, _, err := upsertNodes([]assignmentInput{{Agent: "code-reviewer", Task: "x"}}, nil, nil, "chat1", nil); err != nil {
		t.Errorf("no allowedKinds restriction: %v", err)
	}
	if _, _, err := upsertNodes([]assignmentInput{{Agent: "web-researcher", Task: "x"}}, nil, nil, "chat1", []string{"pull_request"}); err != nil {
		t.Errorf("agent with no delivery coupling: %v", err)
	}
	if _, _, err := upsertNodes([]assignmentInput{{Agent: "code-reviewer", Task: "x"}}, nil, nil, "chat1", []string{"review"}); err != nil {
		t.Errorf("agent's delivery kind IS allowed: %v", err)
	}
}

// TestBuildNodeSummariesResumable covers list_nodes' resumable/reason field:
// a terminal (done) node is resumable, a live one is not regardless of what
// its stored status says (nodeIsRunning outranks it), and a never-run node
// isn't either.
func TestBuildNodeSummariesResumable(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	ctx := context.Background()
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, "quack", "u1", "chat1")
	nodes := []dag.DagNodeRecord{
		{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusDone},
		{NodeID: "impl-2", Agent: "code-implementer", Status: dag.StatusQueued},
		{NodeID: "impl-3", Agent: "code-implementer", Status: dag.StatusDone}, // will report as live below
	}
	for _, n := range nodes {
		if _, _, err := c.SaveStructured(ctx, "dag_node", n, n.NodeID, recordstore.Lineage{}); err != nil {
			t.Fatalf("seed dag_node %s: %v", n.NodeID, err)
		}
	}

	nodeIsRunning := func(id string) bool { return id == "impl-3" }
	out, err := buildNodeSummaries(ctx, c, nodeIsRunning)
	if err != nil {
		t.Fatalf("buildNodeSummaries: %v", err)
	}
	byID := map[string]nodeSummary{}
	for _, s := range out {
		byID[s.NodeID] = s
	}

	if s := byID["impl-1"]; !s.Resumable || s.Reason == "" {
		t.Errorf("impl-1 (done, idle) = %+v, want resumable with a reason", s)
	}
	if s := byID["impl-2"]; s.Resumable {
		t.Errorf("impl-2 (never run) = %+v, want not resumable", s)
	}
	if s := byID["impl-3"]; s.Resumable || s.Reason != "currently running" {
		t.Errorf("impl-3 (live) = %+v, want not resumable, reason \"currently running\"", s)
	}
}
