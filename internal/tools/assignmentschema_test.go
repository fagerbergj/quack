package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// assignments[].agent must be an enum of the current roster in the tool's own emitted schema (what the model
// receives), while staying optional: node_id is the other way to fill it in.
func TestCreatePlanEmitsAgentEnum(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertAssignmentAgentEnum(t, tl.(runnableTool), []string{"code-implementer", "web-researcher"})
}

func TestEditPlanEmitsAgentEnum(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewEditPlanTool(c, "orchestrator", nil, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertAssignmentAgentEnum(t, tl.(runnableTool), []string{"code-implementer", "web-researcher"})
}

// The trigger's setup always overwrites rec.Setup, so a trigger-backed schema describes `setup` as ignored but
// keeps it a known property: dropping it would make ADK reject the whole call. A plain chat leaves it undescribed.
func TestCreatePlanSchemaMarksSetupIgnoredWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), githubSetup)
}

func TestCreatePlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), nil)
}

func TestEditPlanSchemaMarksSetupIgnoredWhenTriggerBacked(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewEditPlanTool(c, "orchestrator", githubSetup, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), githubSetup)
}

func TestEditPlanSchemaKeepsSetupOnPlainChat(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewEditPlanTool(c, "orchestrator", nil, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	assertSetupSchemaDescription(t, tl.(runnableTool), nil)
}

// assertSetupSchemaDescription: with githubSetup, `setup`'s Description says it's ignored and names the
// trigger's repo/base_ref; otherwise it stays empty.
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

// Accepting `setup` on a trigger-backed dispatch must not open AdditionalProperties: a misspelled key is
// still rejected by name.
func TestCreatePlanRejectsMisspelledTopLevelKey(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil, dag.AgentNamesFor(context.Background()))
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

// Prod create_plan (2026-10-13) sent depends_on [0, 1] as JSON numbers: the description defines a
// same-call sibling by its 0-based position, so a number names it as plainly as "0".
func TestCreatePlanAcceptsIntegerDependsOn(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-reviewer"}, {Name: "synthesizer"}}, nil)
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []any{
			map[string]any{"agent": "code-reviewer", "task": "review correctness"},
			map[string]any{"agent": "code-reviewer", "task": "review tests"},
			map[string]any{"agent": "synthesizer", "task": "merge the reviews", "depends_on": []any{0, 1}},
		},
		"delivery": map[string]any{"kind": "review"},
	})
	if err != nil {
		t.Fatalf("create_plan: %v", err)
	}
	synth := res["assignments"].([]any)[2].(map[string]any)
	if got := synth["depends_on"]; !reflect.DeepEqual(got, []any{"code-reviewer-1", "code-reviewer-2"}) {
		t.Errorf("depends_on = %v, want both reviewers' minted ids", got)
	}
}

// Prod create_plan calls (2026-10-12..14) sent null for keys they meant as unset, even one the
// assignment schema lacks; the hooks drop those nulls, while a null required key still fails.
func TestCreatePlanHooksDropRefusedNulls(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-reviewer"}}, nil)
	g := hook(NewHooks(Deps{}, 0), 0, rt)
	_, err := g.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []any{map[string]any{"agent": "code-reviewer", "task": "review PR #1706",
			"delivery": nil, "workdir": nil, "rubric": nil}},
		"delivery": map[string]any{"kind": "review"},
	})
	if err != nil {
		t.Fatalf("create_plan with null optionals: %v", err)
	}
	_, err = g.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []any{map[string]any{"agent": "code-reviewer", "task": nil}},
	})
	if err == nil || !strings.Contains(err.Error(), "task") {
		t.Errorf("null required task: err = %v, want a schema error naming task", err)
	}
}

// assertAssignmentAgentEnum checks assignments[].agent's Enum in tl's actual Declaration() against want,
// and that agent stays optional.
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
