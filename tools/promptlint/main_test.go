package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func fixtureConfig() config {
	c := config{
		Budgets:           map[string]int{"prompt": 60, "rubric": 1000, "skill": 1000, "tool": 100, "agent": 1000},
		ExternalSkills:    []string{"ponytail"},
		OrchestratorTools: []string{"list_artifacts"},
		Ignore: []ignoreRule{{Rule: "missing_resource", Subject: "skills/good-skill/SKILL.md", Match: "gone.md",
			Reason: "fixture example"}},
	}
	c.Overlap.Threshold = 0.3
	c.Overlap.Allow = []allowEntry{
		{Tools: []string{"ask_x", "ask_z"}, Reason: "fixture pair"},
		{Tools: []string{"alpha_tool", "big_tool"}, Reason: "never co-offered"},
	}
	return c
}

func fixtureTools() []toolDef {
	d := func(name, desc string) toolDef { return toolDef{Name: name, Description: desc, Provider: "core"} }
	return []toolDef{
		d("alpha_tool", "Fetch alpha records."),
		d("ask_x", "Ask the user a clarifying question when blocked."),
		d("ask_y", "Ask the user a clarifying question with options to choose from."),
		d("ask_z", "Ask the user a clarifying question and wait."),
		d("big_tool", strings.Repeat("verbose ", 60)),
		d("list_artifacts", "Enumerate stored outputs."),
		d("read_artifact", "Open one by id."),
		d("write_artifact", "Persist a document."),
		d("edit_artifact", "Patch a document by search and replace."),
		d("load_artifacts", "Attach session files."),
		d("list_skills", "Catalog of playbooks."),
		d("load_skill", "Instructions by name."),
		d("load_skill_resource", "A bundled file."),
	}
}

func fixtureRun(changed []string) report {
	return run("testdata/repo", fixtureConfig(), fixtureTools(), changed)
}

func findingFor(rep report, rule, subject, contains string) (finding, bool) {
	for _, f := range rep.Findings {
		if f.Rule == rule && f.Subject == subject && strings.Contains(f.Message, contains) {
			return f, true
		}
	}
	return finding{}, false
}

func TestFindings(t *testing.T) {
	rep := fixtureRun(nil)
	cases := []struct {
		rule, subject, contains string
		present, gated          bool
	}{
		{"budget", "agents/bad/prompt.md", "prompt budget", true, true},
		{"budget", "tool:big_tool", "tool budget", true, true},
		{"missing_link", "agents/bad/prompt.md", "missing-guide.md", true, true},
		{"unknown_skill", "agents/bad/prompt.md", `"nope"`, true, true},
		{"missing_resource", "agents/bad/prompt.md", "references/absent.json", true, true},
		{"missing_resource", "skills/good-skill/SKILL.md", "gone.md", true, false},
		{"rubric_undefined_criterion", "agents/bad/rubric.yaml", "made_up_check", true, true},
		{"rubric_invalid", "agents/worse/rubric.yaml", "pass 5", true, true},
		{"unknown_tool", "agent:bad", "ghost_tool", true, true},
		{"unknown_skill", "agent:bad", "missing-skill", true, true},
		{"unknown_skill", "agent:bad", "ponytail:ponytail-review", true, false},
		{"overlap", "ask_x,ask_y", "bad", true, true},
		{"stale_allowlist", "alpha_tool,big_tool", "", true, false},
		// Clean or waived inputs that must not report.
		{"budget", "agents/good/prompt.md", "", false, false},
		{"missing_link", "agents/good/prompt.md", "", false, false},
		{"unknown_skill", "agents/good/prompt.md", "", false, false},
		{"missing_resource", "agents/bad/prompt.md", "schema.json", false, false},
		{"rubric_undefined_criterion", "agents/good/rubric.yaml", "", false, false},
		{"unknown_tool", "agent:plug", "", false, false},
		{"unknown_skill", "agent:plug", "", false, false},
		{"overlap", "ask_x,ask_z", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.rule+" "+tc.subject+" "+tc.contains, func(t *testing.T) {
			f, ok := findingFor(rep, tc.rule, tc.subject, tc.contains)
			if ok != tc.present {
				t.Fatalf("present = %v, want %v; findings: %+v", ok, tc.present, rep.Findings)
			}
			if ok && f.Gated != tc.gated {
				t.Errorf("gated = %v, want %v: %+v", f.Gated, tc.gated, f)
			}
		})
	}
}

func TestAgentSurfaces(t *testing.T) {
	rep := fixtureRun(nil)
	want := map[string][]string{
		"orchestrator": {"list_artifacts", "write_artifact", "load_artifacts", "list_skills", "load_skill", "load_skill_resource"},
		"good":         {"alpha_tool", "list_artifacts", "read_artifact", "write_artifact", "edit_artifact"},
		"plug":         {"alpha_tool", "ask_x", "ask_z", "list_artifacts", "read_artifact", "list_skills", "load_skill", "load_skill_resource"},
		"coder":        nil,
	}
	for _, a := range rep.Agents {
		w, ok := want[a.Name]
		if !ok {
			continue
		}
		if !slices.Equal(a.Tools, w) {
			t.Errorf("%s tools = %v, want %v", a.Name, a.Tools, w)
		}
		if a.Total != a.PromptTokens+a.ToolTokens+a.SkillTokens {
			t.Errorf("%s total %d is not the sum of its parts", a.Name, a.Total)
		}
	}
	plug := rep.Agents[slices.IndexFunc(rep.Agents, func(a agentSpec) bool { return a.Name == "plug" })]
	if plug.PromptTokens == 0 || plug.SkillTokens == 0 {
		t.Errorf("plug prompt/skill tokens = %d/%d, want the plugin bundle's prompt and demo:s frontmatter", plug.PromptTokens, plug.SkillTokens)
	}
}

// Diff mode gates only findings whose sources the diff touches; editing the config regates everything.
func TestDiffGating(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    []string // rule/subject of every gated finding
	}{
		{"unrelated change gates nothing", []string{"agents/good/prompt.md"}, nil},
		{"rubric change gates its rubric", []string{"agents/bad/rubric.yaml"}, []string{"rubric_undefined_criterion agents/bad/rubric.yaml"}},
		{"tool source change gates its tool and the agents offered it", []string{"internal/tools/x.go"}, []string{
			"budget tool:big_tool", "unknown_tool agent:bad", "unknown_skill agent:bad", "overlap ask_x,ask_y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, f := range fixtureRun(tc.changed).Findings {
				if f.Gated {
					got = append(got, f.Rule+" "+f.Subject)
				}
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Errorf("gated = %v, want %v", got, tc.want)
			}
		})
	}
	full := fixtureRun(nil)
	cfgEdit := fixtureRun([]string{configPath})
	for i := range full.Findings {
		if full.Findings[i].Gated != cfgEdit.Findings[i].Gated {
			t.Errorf("config edit gated %+v differently from a full check", cfgEdit.Findings[i])
		}
	}
}

func TestSummarizeExitsOnGatedFindings(t *testing.T) {
	var buf bytes.Buffer
	if !summarize(&buf, fixtureRun(nil)) || !strings.Contains(buf.String(), "FAIL") {
		t.Fatalf("summary should fail on gated findings:\n%s", buf.String())
	}
	buf.Reset()
	if summarize(&buf, fixtureRun([]string{})) || !strings.Contains(buf.String(), "promptlint: ok") {
		t.Fatalf("an empty diff should pass:\n%s", buf.String())
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"Asks the user", "ask user", 1},
		{"staged facts", "stage fact", 1},
		{"read a file", "grep the web", 0},
		{"alpha beta gamma delta", "alpha beta", 0.5},
	}
	for _, tc := range cases {
		got := jaccard(tokenSet(toolDef{Description: tc.a}), tokenSet(toolDef{Description: tc.b}))
		if got != tc.want {
			t.Errorf("jaccard(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if tokens(0) != 0 || tokens(1) != 1 || tokens(8) != 2 {
		t.Error("tokens must be bytes/4 rounded up")
	}
}

// The checked-in threshold must keep flagging the overlaps that motivated this lint, measured on
// the real declarations.
func TestRealCatalogFlagsKnownOverlaps(t *testing.T) {
	cfg, err := loadConfig("../..")
	if err != nil {
		t.Fatal(err)
	}
	defs, err := catalog()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]toolDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	for _, p := range [][2]string{
		{"ask_user", "get_user_choice"}, {"stage_memory", "commit_memory"},
		{"recall_memory", "load_memory"}, {"read_file", "read_artifact"},
	} {
		a, aok := byName[p[0]]
		b, bok := byName[p[1]]
		if !aok || !bok {
			t.Fatalf("catalog lacks %v", p)
		}
		if s := jaccard(tokenSet(a), tokenSet(b)); s < cfg.Overlap.Threshold {
			t.Errorf("%s ~ %s = %.3f, below the %.2f threshold", p[0], p[1], s, cfg.Overlap.Threshold)
		}
	}
	if byName["web_search"].size() == 0 || byName["sleeper_roster"].Provider != "sleeper" || byName["github_comment"].Provider != "github" {
		t.Error("catalog must measure builtin and extension tools")
	}
}

func TestLoadConfigRejectsReasonlessEntries(t *testing.T) {
	if _, err := loadConfig("testdata/badconfig"); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("err = %v, want a missing-reason error", err)
	}
}
