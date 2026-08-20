package serve

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/tools"
)

type fakeExtTool struct{ name, provider string }

func (f *fakeExtTool) Name() string        { return f.name }
func (f *fakeExtTool) Description() string { return "fake" }
func (f *fakeExtTool) IsLongRunning() bool { return false }
func (f *fakeExtTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: f.name, Description: "fake"}
}
func (f *fakeExtTool) Run(agent.Context, any) (map[string]any, error) {
	return map[string]any{"provider": f.provider}, nil
}

func TestIndexExtToolsNoCollision(t *testing.T) {
	idx := indexExtTools([]extTool{
		{provider: "github", tool: &fakeExtTool{name: "add_comment", provider: "github"}},
	})
	if got := idx["add_comment"]; got == nil || got.Name() != "add_comment" {
		t.Fatalf("bare name should resolve unchanged, got %v", got)
	}
	// Prefixed form works even without a collision.
	p := idx["github_add_comment"]
	if p == nil || p.Name() != "github_add_comment" {
		t.Fatalf("prefixed form should resolve, got %v", p)
	}
	if p.(*renamedTool).Declaration().Name != "github_add_comment" {
		t.Fatal("prefixed alias must declare its prefixed name to the model")
	}
}

func TestIndexExtToolsCollision(t *testing.T) {
	idx := indexExtTools([]extTool{
		{provider: "github", tool: &fakeExtTool{name: "search", provider: "github"}},
		{provider: "jira", tool: &fakeExtTool{name: "search", provider: "jira"}},
	})
	for _, name := range []string{"github_search", "jira_search"} {
		tl, ok := idx[name]
		if !ok || tl == nil || tl.Name() != name {
			t.Fatalf("%s should resolve to a renamed tool, got %v", name, tl)
		}
	}
	// Each prefixed alias routes to its own provider's tool.
	got, err := idx["jira_search"].(*renamedTool).Run(nil, nil)
	if err != nil || got["provider"] != "jira" {
		t.Fatalf("jira_search should run the jira tool, got %v, %v", got, err)
	}
	// Bare name is a nil sentinel: ambiguous, never a silent pick.
	if tl, ok := idx["search"]; !ok || tl != nil {
		t.Fatalf("collided bare name should be the ambiguity sentinel, got %v (present=%v)", tl, ok)
	}
}

func TestBuildRejectsAmbiguousBareName(t *testing.T) {
	idx := indexExtTools([]extTool{
		{provider: "github", tool: &fakeExtTool{name: "search", provider: "github"}},
		{provider: "jira", tool: &fakeExtTool{name: "search", provider: "jira"}},
	})
	if _, err := tools.Build([]string{"search"}, tools.Deps{ExtTools: idx}); err == nil || !strings.Contains(err.Error(), "more than one extension") {
		t.Fatalf("bare collided name in a tools: list should error as ambiguous, got %v", err)
	}
	built, err := tools.Build([]string{"github_search"}, tools.Deps{ExtTools: idx})
	if err != nil || len(built) != 1 {
		t.Fatalf("prefixed name should build, got %v, %v", built, err)
	}
}

var _ tool.Tool = (*fakeExtTool)(nil)
