package promptbuilder_test

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/promptbuilder"
	"github.com/fagerbergj/quack/internal/tools"
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
		{"writing layer", "# Writing ruleset"},
		{"environment header", "## Environment"},
		{"environment today", "Today is"},
	}
	for _, c := range cases {
		if !strings.Contains(out, c.want) {
			t.Errorf("Agent() missing %s layer: %q not in output", c.layer, c.want)
		}
	}
}

// TestWritingLayerAlways verifies the shared prose ruleset is injected even for
// a bare agent (no tools, no behaviour) - it applies to every assembled prompt.
func TestWritingLayerAlways(t *testing.T) {
	for _, out := range []string{
		promptbuilder.Agent("helper", "helps", nil, ""),
		promptbuilder.Judge(nil, ""),
		promptbuilder.Orchestrator("", nil, ""),
	} {
		if !strings.Contains(out, "# Writing ruleset") {
			t.Error("assembled prompt missing the Writing layer")
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

// toolLine is how promptbuilder renders a tool in the ## Tools section
// ("- `name` - desc"). Asserting this exact prefix proves the tool is in the
// Tools list, distinct from a tool name merely mentioned in the behaviour/guidance.
func toolLine(name string) string { return "- `" + name + "` -" }

// TestAgentMemoryToolsAndGuidance verifies the M6 memory tools AND the memory.md
// guidance both reach the assembled prompt - the way agent.Build wires them
// (memory tools in the tool list; memory.md appended to the behaviour).
func TestAgentMemoryToolsAndGuidance(t *testing.T) {
	memTools := []tool.Tool{
		fakeTool{"web_search", "searches the web"},
		fakeTool{"stage_memory", "stage tradecraft"},
		fakeTool{"load_memory", "deliberate recall"},
		fakeTool{"preload_memory", "ambient recall"},
	}
	// Build sets behaviour = prompt.md + "\n\n" + memory.md; mirror that here.
	behaviour := "## Steps\n1. Plan." + "\n\n" + "## What to remember\n\nStage durable tradecraft."
	out := promptbuilder.Agent("web-researcher", "researches the web", memTools, behaviour)

	for _, name := range []string{"stage_memory", "load_memory", "preload_memory"} {
		if !strings.Contains(out, toolLine(name)) {
			t.Errorf("Agent() Tools section missing memory tool %q", name)
		}
	}
	if !strings.Contains(out, "## What to remember") {
		t.Error("Agent() missing memory.md guidance (## What to remember)")
	}
}

// TestAgentMemoryRealBundle is an end-to-end check that the REAL web-researcher
// memory.md and the REAL memory tools both render in its prompt - using the same
// loader (agent.LoadBundleMemory) and tools (stage_memory builtin + ADK
// load/preload) that buildAgents wires when memory is enabled.
func TestAgentMemoryRealBundle(t *testing.T) {
	const dir = "../../agents/web-researcher"
	bundle, err := agent.LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	mem, err := agent.LoadBundleMemory(dir)
	if err != nil {
		t.Fatalf("LoadBundleMemory: %v", err)
	}
	if strings.TrimSpace(mem) == "" {
		t.Fatal("web-researcher has no memory.md - memory guidance would never load")
	}

	builtins, err := tools.Build([]string{"stage_memory"}, tools.Deps{})
	if err != nil {
		t.Fatalf("tools.Build(stage_memory): %v", err)
	}
	memTools := append(builtins, loadmemorytool.New(), preloadmemorytool.New())

	behaviour := bundle.Prompt + "\n\n" + mem // exactly what agent.Build assembles
	out := promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, memTools, behaviour)

	// Both deliberate memory tools must appear in the Tools section.
	for _, name := range []string{"stage_memory", "load_memory", "preload_memory"} {
		if !strings.Contains(out, toolLine(name)) {
			t.Errorf("web-researcher prompt missing memory tool %q in the Tools section", name)
		}
	}
	// The memory.md guidance must be present (heading is unique to the file).
	if !strings.Contains(out, "## What to remember") {
		t.Error("web-researcher prompt missing memory.md guidance")
	}
}

// TestOrchestratorLayers verifies each prompt layer appears in the output.
func TestOrchestratorLayers(t *testing.T) {
	frontmatters := []*skill.Frontmatter{
		{Name: "format-markdown", Description: "reformats markdown"},
	}
	roster := "- `web-researcher` - searches the web\n"
	out := promptbuilder.Orchestrator(roster, frontmatters, "## Steps\n1. Understand.")

	cases := []struct {
		layer string
		want  string
	}{
		{"identity", "orchestrator"},
		{"agents header", "### Agents"},
		{"agent name", "web-researcher"},
		{"skills header", "### Skills"},
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
	out := promptbuilder.Orchestrator("", nil, "do stuff")
	if strings.Contains(out, "### Skills") {
		t.Error("Orchestrator() should not emit a Skills section when no skills provided")
	}
}
