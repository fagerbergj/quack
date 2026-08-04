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
	out := promptbuilder.Agent("web-researcher", "researches the web", tools, nil, "## Steps\n1. Plan.", "", "")

	cases := []struct {
		layer string
		want  string
	}{
		{"identity name", "web-researcher"},
		{"identity description", "researches the web"},
		{"capabilities header", "## Capabilities"},
		{"tools header", "### Tools"},
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

// TestAgentSkillsRendered verifies a specialist agent's declared skill scope
// renders as a Skills subsection (A1 - previously only Orchestrator did this;
// a specialist had no way to learn its bound skill names exist at all).
func TestAgentSkillsRendered(t *testing.T) {
	skills := []*skill.Frontmatter{
		{Name: "research-git-repos", Description: "clone and read a repo locally"},
	}
	out := promptbuilder.Agent("web-researcher", "researches the web", nil, skills, "", "", "")

	for _, want := range []string{"### Skills", "load_skill", "research-git-repos", "clone and read a repo locally"} {
		if !strings.Contains(out, want) {
			t.Errorf("Agent() with skills missing %q in output:\n%s", want, out)
		}
	}
}

// TestAgentACPNoFabricatedTools verifies the ACP shape (nil tools, real
// skills) never fabricates a Tools section - an external coding agent has no
// quack tools, so the Capabilities layer must carry only what's real.
func TestAgentACPNoFabricatedTools(t *testing.T) {
	skills := []*skill.Frontmatter{{Name: "ponytail", Description: "laziest thing that works"}}
	out := promptbuilder.Agent("code-implementer", "implements code", nil, skills, "## Ground rules\nCommit atomically.", "", "")

	if strings.Contains(out, "### Tools") {
		t.Error("Agent() with nil tools should never emit a ### Tools section")
	}
	for _, want := range []string{"### Skills", "ponytail", "## Ground rules", "## Capabilities"} {
		if !strings.Contains(out, want) {
			t.Errorf("Agent() ACP shape missing %q in output:\n%s", want, out)
		}
	}
}

// TestGradingFacts verifies the grading block states only what applies to a
// given agent, sourced from its resolved gate config - never a fabricated
// number - and is entirely absent for a judge-less agent.
func TestGradingFacts(t *testing.T) {
	g := promptbuilder.GradingFacts(0.7, 2, false, true)
	for _, want := range []string{"0.7", "weakest-link", "2 revision round"} {
		if !strings.Contains(g, want) {
			t.Errorf("GradingFacts missing %q in: %q", want, g)
		}
	}
	if !strings.Contains(g, "zero retrieval activity") {
		t.Error("GradingFacts should state the retrieval requirement when requireRetrieval is true")
	}
	if strings.Contains(g, "delivery tools") {
		t.Error("GradingFacts should not state the read-only fact when readOnly is false")
	}

	if got := promptbuilder.GradingFacts(0.7, 0, false, false); got != "" {
		t.Errorf("GradingFacts with judgeRounds=0 should be empty (no judge to score this agent), got %q", got)
	}
}

// TestAgentGradingRendered verifies a non-empty grading block reaches the
// assembled prompt as its own layer.
func TestAgentGradingRendered(t *testing.T) {
	grading := promptbuilder.GradingFacts(0.7, 1, true, false)
	out := promptbuilder.Agent("code-reviewer", "reviews code", nil, nil, "", grading, "")

	if !strings.Contains(out, "## Grading") {
		t.Error("Agent() with a non-empty grading fact should emit a ## Grading section")
	}
	if !strings.Contains(out, "delivery tools") {
		t.Error("Agent() grading section missing the read-only fact")
	}
}

func TestAgentNoGrading(t *testing.T) {
	out := promptbuilder.Agent("helper", "helps", nil, nil, "do stuff", "", "")
	if strings.Contains(out, "## Grading") {
		t.Error("Agent() should not emit ## Grading when grading is empty")
	}
}

// TestWritingLayerAlways verifies the shared prose ruleset is injected even for
// a bare agent (no tools, no behaviour) - it applies to every assembled prompt.
func TestWritingLayerAlways(t *testing.T) {
	for _, out := range []string{
		promptbuilder.Agent("helper", "helps", nil, nil, "", "", ""),
		promptbuilder.Judge(nil, ""),
		promptbuilder.Orchestrator("", nil, ""),
	} {
		if !strings.Contains(out, "# Writing ruleset") {
			t.Error("assembled prompt missing the Writing layer")
		}
	}
}

func TestAgentNoTools(t *testing.T) {
	out := promptbuilder.Agent("helper", "helps", nil, nil, "do stuff", "", "")
	if strings.Contains(out, "### Tools") {
		t.Error("Agent() should not emit ### Tools section when no tools provided")
	}
	if !strings.Contains(out, "## Environment") {
		t.Error("Agent() must always include ## Environment layer")
	}
}

func TestAgentNoBehaviour(t *testing.T) {
	out := promptbuilder.Agent("helper", "helps", nil, nil, "", "", "")
	if !strings.Contains(out, "## Environment") {
		t.Error("Agent() must include ## Environment even with empty behaviour")
	}
}

// toolLine is how promptbuilder renders a tool in the ### Tools section
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
	out := promptbuilder.Agent("web-researcher", "researches the web", memTools, nil, behaviour, "", "")

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
	out := promptbuilder.Agent(bundle.Card.Name, bundle.Card.Description, memTools, nil, behaviour, "", "")

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

// TestAgentWorkspaceRendered verifies a non-empty workspace block reaches the
// Environment layer, and that a non-coding agent (workspace == "") never
// fabricates one - Agent's own callers decide which is which (build.go
// always passes "", the ACP branch always passes workspace.PromptBlock).
func TestAgentWorkspaceRendered(t *testing.T) {
	out := promptbuilder.Agent("code-implementer", "implements code", nil, nil, "", "", "Linux x86_64. Sandbox: landlock (…).")
	if !strings.Contains(out, "## Environment") {
		t.Fatal("Agent() with workspace facts should still emit ## Environment")
	}
	if !strings.Contains(out, "Linux x86_64. Sandbox: landlock (…).") {
		t.Error("Agent() should render the workspace block verbatim in the Environment layer")
	}

	out = promptbuilder.Agent("web-researcher", "researches the web", nil, nil, "", "", "")
	if strings.Contains(out, "Sandbox:") {
		t.Error("Agent() with an empty workspace block should never fabricate one")
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
