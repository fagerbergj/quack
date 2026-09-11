package tools

import (
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestCreatePlanEmitsAgentEnum and TestEditPlanEmitsAgentEnum pin the rig
// regression (#slice3 review): a small model omitted `agent` in most of its
// create_plan calls even after the roster was named in the rejection text.
// assignments[].agent must be an enum of the current roster in the tool's
// OWN emitted schema (Declaration(), what the model actually receives) - a
// contract the tool enforces, not a prompt hint - while staying optional
// (node_id is the other valid way to fill it in).
func TestCreatePlanEmitsAgentEnum(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertAssignmentAgentEnum(t, tl.(runnableTool), []string{"code-implementer", "web-researcher"})
}

func TestEditPlanEmitsAgentEnum(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewEditPlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertAssignmentAgentEnum(t, tl.(runnableTool), []string{"code-implementer", "web-researcher"})
}

// assertAssignmentAgentEnum digs assignments[].agent's Enum out of tl's
// actual Declaration() - what functiontool.New resolved and the model
// receives, not just assignmentInputSchema's own return value - and checks
// it against want, plus that agent stays optional (never in Required).
func assertAssignmentAgentEnum(t *testing.T, tl runnableTool, want []string) {
	t.Helper()
	decl := tl.Declaration()
	schema, ok := decl.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("ParametersJsonSchema = %T, want *jsonschema.Schema", decl.ParametersJsonSchema)
	}
	assignments, ok := schema.Properties["assignments"]
	if !ok || assignments.Items == nil {
		t.Fatalf("schema has no assignments[].items: %+v", schema.Properties)
	}
	agentProp, ok := assignments.Items.Properties["agent"]
	if !ok {
		t.Fatalf("assignments[].items has no agent property: %+v", assignments.Items.Properties)
	}
	if len(agentProp.Enum) != len(want) {
		t.Fatalf("agent enum = %v, want %v", agentProp.Enum, want)
	}
	for i, w := range want {
		if agentProp.Enum[i] != w {
			t.Fatalf("agent enum = %v, want %v", agentProp.Enum, want)
		}
	}
	for _, req := range assignments.Items.Required {
		if req == "agent" {
			t.Fatalf("agent is Required = %+v, want it to stay optional (node_id is the other valid way to fill it in)", assignments.Items.Required)
		}
	}
}
