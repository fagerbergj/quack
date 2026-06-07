package promptbuilder_test

import (
	"strings"
	"testing"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// fakeTool satisfies tool.Tool for testing.
type fakeTool struct{ name, desc string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return f.desc }
func (f fakeTool) IsLongRunning() bool { return false }

var _ tool.Tool = fakeTool{}

// TestAgentLayers verifies each prompt layer appears in the output.
func TestAgentLayers(t *testing.T) {
	tools := []tool.Tool{
		fakeTool{"web_search", "searches the web"},
		fakeTool{"web_fetch", "fetches a URL"},
	}
	out := promptbuilder.Agent("web-researcher", "researches the web", tools, "## Steps\n1. Plan.")

	cases := []struct {
		layer string
		want  string
	}{
		{"identity name", "web-researcher"},
		{"identity description", "researches the web"},
		{"tools header", "## Tools"},
		{"tool name", "web_search"},
		{"tool description", "searches the web"},
		{"behaviour", "## Steps"},
		{"environment header", "## Environment"},
		{"environment today", "Today is"},
	}
	for _, c := range cases {
		if !strings.Contains(out, c.want) {
			t.Errorf("Agent() missing %s layer: %q not in output", c.layer, c.want)
		}
	}
}

func TestAgentNoTools(t *testing.T) {
	out := promptbuilder.Agent("helper", "helps", nil, "do stuff")
	if strings.Contains(out, "## Tools") {
		t.Error("Agent() should not emit ## Tools section when no tools provided")
	}
	if !strings.Contains(out, "## Environment") {
		t.Error("Agent() must always include ## Environment layer")
	}
}

func TestAgentNoBehaviour(t *testing.T) {
	out := promptbuilder.Agent("helper", "helps", nil, "")
	if !strings.Contains(out, "## Environment") {
		t.Error("Agent() must include ## Environment even with empty behaviour")
	}
}

// TestOrchestratorLayers verifies each prompt layer appears in the output.
func TestOrchestratorLayers(t *testing.T) {
	frontmatters := []*skill.Frontmatter{
		{Name: "format-markdown", Description: "reformats markdown"},
	}
	out := promptbuilder.Orchestrator(frontmatters, "## Steps\n1. Understand.")

	cases := []struct {
		layer string
		want  string
	}{
		{"identity", "orchestrator"},
		{"skills header", "## Skills"},
		{"skill name", "format-markdown"},
		{"skill description", "reformats markdown"},
		{"load_skill hint", "load_skill"},
		{"behaviour", "## Steps"},
		{"environment header", "## Environment"},
		{"environment today", "Today is"},
	}
	for _, c := range cases {
		if !strings.Contains(out, c.want) {
			t.Errorf("Orchestrator() missing %s layer: %q not in output", c.layer, c.want)
		}
	}
}

func TestOrchestratorNoSkills(t *testing.T) {
	out := promptbuilder.Orchestrator(nil, "do stuff")
	if strings.Contains(out, "## Skills") {
		t.Error("Orchestrator() should not emit ## Skills section when no skills provided")
	}
}
