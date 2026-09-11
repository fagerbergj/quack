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

// TestCreatePlanSchemaOmitsSetupWhenTriggerBacked and
// TestEditPlanSchemaOmitsSetupWhenTriggerBacked pin the other half of the
// rig regression (#slice3 review): the trigger's own setup always
// overwrites rec.Setup regardless of what's submitted, so a trigger-backed
// dispatch's schema must not even offer `setup` - a property the model
// can't actually change only invites a wrong guess (the rig's own repro:
// rejected on setup.repo, then the corrected retry dropped `agent`
// instead). A plain chat (no trigger) keeps `setup`.
func TestCreatePlanSchemaOmitsSetupWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaPresence(t, tl.(runnableTool), false)
}

func TestCreatePlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaPresence(t, tl.(runnableTool), true)
}

func TestEditPlanSchemaOmitsSetupWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewEditPlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaPresence(t, tl.(runnableTool), false)
}

func TestEditPlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewEditPlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaPresence(t, tl.(runnableTool), true)
}

func assertSetupSchemaPresence(t *testing.T, tl runnableTool, wantPresent bool) {
	t.Helper()
	decl := tl.Declaration()
	schema, ok := decl.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("ParametersJsonSchema = %T, want *jsonschema.Schema", decl.ParametersJsonSchema)
	}
	_, present := schema.Properties["setup"]
	if present != wantPresent {
		t.Errorf("schema.Properties[setup] present = %v, want %v (properties: %v)", present, wantPresent, schema.Properties)
	}
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
