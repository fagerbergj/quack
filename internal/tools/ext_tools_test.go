package tools

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// stubExtTool builds a minimal tool.Tool standing in for an extension tool
// (e.g. internal/github.App.Tools()'s github_add_review_comment) - Build must
// treat it exactly like a registry entry, resolved ONLY by name.
func stubExtTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[struct{}, string](
		functiontool.Config{Name: name, Description: "stub"},
		func(_ adkagent.Context, _ struct{}) (string, error) { return "ok", nil },
	)
	if err != nil {
		t.Fatalf("stubExtTool(%q): %v", name, err)
	}
	return tl
}

func hasTool(tools []tool.Tool, name string) bool {
	for _, tl := range tools {
		if tl.Name() == name {
			return true
		}
	}
	return false
}

// TestBuildExtToolsOptIn guards the fix for the force-injection design bug: an
// extension tool (Deps.ExtTools) reaches an agent ONLY when that agent's own
// config tools: list names it - same resolution path as any builtin - never
// because the extension happens to be configured at all.
func TestBuildExtToolsOptIn(t *testing.T) {
	ext := map[string]tool.Tool{
		"github_add_review_comment": stubExtTool(t, "github_add_review_comment"),
	}

	// A tools: list that omits the ext tool must not receive it.
	got, err := Build([]string{"current_date"}, Deps{ExtTools: ext})
	if err != nil {
		t.Fatalf("Build without the ext tool named: %v", err)
	}
	if hasTool(got, "github_add_review_comment") {
		t.Error("ext tool present even though tools: never named it - force-injection regressed")
	}

	// A tools: list that names the ext tool must receive it, through the same
	// guard/scrub/cancel pipeline as a builtin.
	got, err = Build([]string{"github_add_review_comment"}, Deps{ExtTools: ext})
	if err != nil {
		t.Fatalf("Build with the ext tool named: %v", err)
	}
	if !hasTool(got, "github_add_review_comment") {
		t.Error("ext tool absent even though tools: named it")
	}

	// A name in neither the registry nor ExtTools is still an error.
	if _, err := Build([]string{"github_comment"}, Deps{ExtTools: ext}); err == nil {
		t.Error("Build(unregistered ext name) should error like any unknown builtin")
	}
}
