package tools

import (
	"strings"
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

// TestCreatePlanSchemaMarksSetupIgnoredWhenTriggerBacked and
// TestEditPlanSchemaMarksSetupIgnoredWhenTriggerBacked pin the other half
// of the rig regression (#slice3 review): the trigger's own setup always
// overwrites rec.Setup regardless of what's submitted, so a trigger-backed
// dispatch's schema stops ADVERTISING `setup` as useful (a Description
// saying it's ignored) - it stays a known property (so a model that sends
// it anyway is still schema-valid; deleting it outright would make ADK's
// own schema validation reject the whole call before setupIgnoredNote ever
// runs). A plain chat (no trigger) leaves `setup` undescribed.
func TestCreatePlanSchemaMarksSetupIgnoredWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), githubSetup)
}

func TestCreatePlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), nil)
}

func TestEditPlanSchemaMarksSetupIgnoredWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewEditPlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), githubSetup)
}

func TestEditPlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewEditPlanTool(c, "orchestrator", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), nil)
}

// assertSetupSchemaDescription checks `setup` stays a known property either
// way; when githubSetup is non-nil, its Description must say it's ignored
// and name the trigger's own repo/base_ref, otherwise it must be untouched
// (empty - jsonschema.For never sets one for a bare field with no doc tag).
func assertSetupSchemaDescription(t *testing.T, tl runnableTool, githubSetup *dag.Setup) {
	t.Helper()
	decl := tl.Declaration()
	schema, ok := decl.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("ParametersJsonSchema = %T, want *jsonschema.Schema", decl.ParametersJsonSchema)
	}
	setupProp, present := schema.Properties["setup"]
	if !present {
		t.Fatalf("schema.Properties[setup] missing - it must stay a known property so a model that sends it is still schema-valid")
	}
	if githubSetup == nil {
		if setupProp.Description != "" {
			t.Errorf("setup.Description = %q, want empty on a plain chat", setupProp.Description)
		}
		return
	}
	if !strings.Contains(strings.ToLower(setupProp.Description), "ignored") || !strings.Contains(setupProp.Description, githubSetup.Repo) {
		t.Errorf("setup.Description = %q, want it to say ignored and name the trigger's repo", setupProp.Description)
	}
}

// TestCreatePlanRejectsMisspelledTopLevelKey pins the reviewer's correction
// (#slice3 review): widening AdditionalProperties to accommodate a model
// that still sends `setup` on a trigger-backed dispatch must not also let a
// genuinely misspelled key silently pass schema validation and get dropped
// - it stays closed (jsonschema.For's own default), only `setup` itself
// gets a description, so an unknown key is still rejected by name.
func TestCreatePlanRejectsMisspelledTopLevelKey(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignmnets": []map[string]any{{"agent": "web-researcher", "task": "x"}}, // misspelled "assignments"
	})
	if err == nil {
		t.Fatal("want an error for a misspelled top-level key, not silent acceptance")
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
