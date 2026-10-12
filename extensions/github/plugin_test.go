package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readPluginFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("plugin", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rubricCriterionText flattens one criterion's strings, whitespace-collapsed, so a marker split across lines still matches.
func rubricCriterionText(t *testing.T, agent, criterion string) string {
	t.Helper()
	var doc struct {
		Criteria map[string]any `yaml:"criteria"`
	}
	if err := yaml.Unmarshal([]byte(readPluginFile(t, "agents/"+agent+"/rubric.yaml")), &doc); err != nil {
		t.Fatal(err)
	}
	c, ok := doc.Criteria[criterion]
	if !ok {
		t.Fatalf("%s rubric has no %s criterion", agent, criterion)
	}
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			parts = append(parts, v)
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(c)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func requireAll(t *testing.T, what, body string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if !strings.Contains(body, m) {
			t.Errorf("%s missing %q", what, m)
		}
	}
}

// TestPluginAgentsShipCardPromptRubric: every listed agent's card names its own directory,
// since quack keys gate configs by the card name.
func TestPluginAgentsShipCardPromptRubric(t *testing.T) {
	var manifest struct {
		Extensions map[string]struct {
			Agents []string `json:"agents"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(readPluginFile(t, "plugin.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	agents := manifest.Extensions["io.github.fagerbergj.quack"].Agents
	if len(agents) != 3 {
		t.Fatalf("plugin.json lists %v, want the three code agents", agents)
	}
	for _, name := range agents {
		var card struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(readPluginFile(t, "agents/"+name+"/agent-card.json")), &card); err != nil {
			t.Fatal(err)
		}
		if card.Name != name {
			t.Errorf("%s: card name %q", name, card.Name)
		}
		for _, f := range []string{"prompt.md", "rubric.yaml", "agent.yaml"} {
			readPluginFile(t, "agents/"+name+"/"+f)
		}
	}
}

// TestBundlePromptsDoNotAskAgentToCheckToolExistence: an ACP subprocess can't prove an MCP tool absent;
// quack's preamble states the offered names as fact (quack #688).
func TestBundlePromptsDoNotAskAgentToCheckToolExistence(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)check your (actual )?tool list|not in your tool list`)
	for _, name := range []string{"code-reviewer", "code-implementer", "code-explorer"} {
		if forbidden.MatchString(readPluginFile(t, "agents/"+name+"/prompt.md")) {
			t.Errorf("%s/prompt.md tells the agent to check its own tool list", name)
		}
	}
}

// TestCodeImplementerBundle: the anti-fabrication rule pairs with the judge's claims_match_activity criterion.
func TestCodeImplementerBundle(t *testing.T) {
	requireAll(t, "code-implementer rubric", readPluginFile(t, "agents/code-implementer/rubric.yaml"),
		"checks_pass", "complexity_proportionate", "module_shape", "coupling", "claims_match_activity", "Workspace activity", "ledger")
	requireAll(t, "code-implementer prompt", readPluginFile(t, "agents/code-implementer/prompt.md"),
		"Report only what actually happened", "you never push or open the PR")
}

// TestCodeReviewerBundlePrefersStaging: staging is the imperative and the structured tail a fallback,
// or the reviewer writes every finding twice.
func TestCodeReviewerBundlePrefersStaging(t *testing.T) {
	prompt := readPluginFile(t, "agents/code-reviewer/prompt.md")
	requireAll(t, "code-reviewer prompt", prompt,
		"Stage the review as you go", "call `stage_review_comment`",
		"has no `stage_review_comment`/`stage_review`",
		"restatement of findings already staged inline",
		"Now I have a complete picture", "Let me compile my findings")
	if strings.Contains(prompt, "the two never conflict") {
		t.Error("prompt still invites writing both the staged review and the tail")
	}
}

// TestCodeReviewerPromptScopesSliceVerdict: a plan-work slice never stages a verdict; the synthesizer owns it.
func TestCodeReviewerPromptScopesSliceVerdict(t *testing.T) {
	body := readPluginFile(t, "agents/code-reviewer/prompt.md")
	if !strings.Contains(body, "YOUR SLICE") {
		t.Fatal("prompt.md should reference the plan-work slice task phrasing")
	}
	if !strings.Contains(body, "do NOT call `stage_review`") && !strings.Contains(body, "do not stage an overall verdict") {
		t.Fatal("prompt.md must tell a slice-scoped reviewer to stage findings only, never a verdict")
	}
}

// TestStructuredVerdictRubricCatchesSeverityCoherence: both directions of a severity/verdict contradiction are named.
func TestStructuredVerdictRubricCatchesSeverityCoherence(t *testing.T) {
	s := rubricCriterionText(t, "code-reviewer", "structured_verdict")
	requireAll(t, "structured_verdict", s, "blocking", "approve", "request_changes", "nit")
	if !strings.Contains(s, "own severity label") && !strings.Contains(s, "OWN severity label") {
		t.Error("structured_verdict does not cross-check each finding's own severity label against the verdict")
	}
}

// TestConstructiveActionableRubricScoresCodeBlocks: proposed code goes in a plain fenced block, never
// ```suggestion; observational findings are exempt.
func TestConstructiveActionableRubricScoresCodeBlocks(t *testing.T) {
	s := rubricCriterionText(t, "code-reviewer", "constructive_actionable")
	requireAll(t, "constructive_actionable", s, "fenced code block")
	if strings.Contains(s, "suggestion") {
		t.Error("constructive_actionable must not reference GitHub ```suggestion blocks")
	}
	if !strings.Contains(s, "PURELY OBSERVATIONAL") && !strings.Contains(s, "purely observational") {
		t.Error("constructive_actionable does not exempt purely observational findings")
	}
	if !strings.Contains(s, "NO code block") && !strings.Contains(s, "no code block") {
		t.Error("constructive_actionable does not say observational findings need no code block")
	}
}
