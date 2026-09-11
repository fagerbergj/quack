package serve

import (
	"context"
	"testing"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/fagerbergj/quack/internal/dag"
)

// githubStubExt stubs the GitHub extension's real implementation of the two
// node-reuse hooks - the actual GitHub-side behaviour (a live base_sha
// comparison, a real branch tip lookup) lands in quack-extensions (ext PR
// #84), not here; this only proves quack's own optional-interface detection,
// wiring, and dag.Assignment -> sdk.Assignment conversion.
type githubStubExt struct {
	freshnessCalls []extsdk.Assignment
	freshnessFresh bool
	freshnessWhy   string
	metaCalls      []extsdk.Assignment
	metaOut        map[string]any
}

func (githubStubExt) Tools() []tool.Tool                    { return nil }
func (githubStubExt) RegisterRoutes(chi.Router, chi.Router) {}

func (e *githubStubExt) BeforeAssignment(_ context.Context, a extsdk.Assignment) (bool, string) {
	e.freshnessCalls = append(e.freshnessCalls, a)
	return e.freshnessFresh, e.freshnessWhy
}

func (e *githubStubExt) OnAssignment(_ context.Context, a extsdk.Assignment) map[string]any {
	e.metaCalls = append(e.metaCalls, a)
	return e.metaOut
}

// plainExt implements only the base extsdk.Extension surface - neither hook.
type plainExt struct{}

func (plainExt) Tools() []tool.Tool                    { return nil }
func (plainExt) RegisterRoutes(chi.Router, chi.Router) {}

func TestFindAssignmentFreshnessChecker(t *testing.T) {
	stub := &githubStubExt{freshnessFresh: false, freshnessWhy: "branch moved"}
	exts := []builtSDKExtension{{name: "plain", ext: plainExt{}}, {name: "github", ext: stub}}

	found, name := findAssignmentFreshnessChecker(exts)
	if found == nil {
		t.Fatal("want the stub detected via its optional BeforeAssignment method")
	}
	if name != "github" {
		t.Errorf("name = %q, want %q", name, "github")
	}
	fresh, reason := found.BeforeAssignment(context.Background(), extsdk.Assignment{NodeID: "impl-1"})
	if fresh || reason != "branch moved" {
		t.Errorf("BeforeAssignment = (%v, %q), want (false, \"branch moved\")", fresh, reason)
	}
	if len(stub.freshnessCalls) != 1 || stub.freshnessCalls[0].NodeID != "impl-1" {
		t.Errorf("freshnessCalls = %+v, want exactly the one call forwarded", stub.freshnessCalls)
	}
}

func TestFindAssignmentFreshnessChecker_NoneImplement(t *testing.T) {
	exts := []builtSDKExtension{{name: "plain", ext: plainExt{}}}
	found, name := findAssignmentFreshnessChecker(exts)
	if found != nil || name != "" {
		t.Errorf("found=%v name=%q, want none detected", found, name)
	}
}

func TestFindAssignmentMetaExtension(t *testing.T) {
	stub := &githubStubExt{metaOut: map[string]any{"base_sha": "abc123"}}
	exts := []builtSDKExtension{{name: "github", ext: stub}}

	found, name := findAssignmentMetaExtension(exts)
	if found == nil {
		t.Fatal("want the stub detected via its optional OnAssignment method")
	}
	if name != "github" {
		t.Errorf("name = %q, want %q", name, "github")
	}
	meta := found.OnAssignment(context.Background(), extsdk.Assignment{NodeID: "impl-1"})
	if meta["base_sha"] != "abc123" {
		t.Errorf("meta = %+v, want base_sha forwarded", meta)
	}
}

// TestToSDKAssignment covers the one place quack's internal dag.Assignment
// crosses into the sdk's wire shape - every field either copied straight
// across or supplied by the caller (planID/agentName/contextID, none of
// which live on dag.Assignment itself).
func TestToSDKAssignment(t *testing.T) {
	a := dag.Assignment{
		NodeID: "impl-1", Task: "do the thing", DependsOn: []string{"web-researcher-1"},
		TaskID: "task-abc", Meta: map[string]map[string]any{"github": {"base_sha": "deadbeef"}},
	}
	got := toSDKAssignment("plan-1", "code-implementer", "pi-xyz", a)
	want := extsdk.Assignment{
		PlanID: "plan-1", NodeID: "impl-1", Agent: "code-implementer", Task: "do the thing",
		DependsOn: []string{"web-researcher-1"}, ContextID: "pi-xyz", TaskID: "task-abc",
		Meta: map[string]map[string]any{"github": {"base_sha": "deadbeef"}},
	}
	if got.PlanID != want.PlanID || got.NodeID != want.NodeID || got.Agent != want.Agent ||
		got.Task != want.Task || got.ContextID != want.ContextID || got.TaskID != want.TaskID ||
		len(got.DependsOn) != 1 || got.DependsOn[0] != want.DependsOn[0] ||
		got.Meta["github"]["base_sha"] != "deadbeef" {
		t.Errorf("toSDKAssignment = %+v, want %+v", got, want)
	}
}
