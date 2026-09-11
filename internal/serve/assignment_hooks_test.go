package serve

import (
	"context"
	"testing"

	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/dag"
)

// githubStubExt stubs the GitHub extension's real implementation of the two
// node-reuse hooks - the actual GitHub-side behaviour (a live base_sha
// comparison, a real branch tip lookup) lands in quack-extensions, not here;
// this only proves quack's own optional-interface detection and wiring.
type githubStubExt struct {
	freshnessCalls []dag.Assignment
	freshnessFresh bool
	freshnessWhy   string
	metaCalls      []dag.Assignment
	metaOut        map[string]any
}

func (githubStubExt) Tools() []tool.Tool                    { return nil }
func (githubStubExt) RegisterRoutes(chi.Router, chi.Router) {}

func (e *githubStubExt) BeforeAssignment(_ context.Context, a dag.Assignment) (bool, string) {
	e.freshnessCalls = append(e.freshnessCalls, a)
	return e.freshnessFresh, e.freshnessWhy
}

func (e *githubStubExt) OnAssignment(_ context.Context, a dag.Assignment) map[string]any {
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
	fresh, reason := found.BeforeAssignment(context.Background(), dag.Assignment{NodeID: "impl-1"})
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
	meta := found.OnAssignment(context.Background(), dag.Assignment{NodeID: "impl-1"})
	if meta["base_sha"] != "abc123" {
		t.Errorf("meta = %+v, want base_sha forwarded", meta)
	}
}
