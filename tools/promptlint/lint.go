package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/tools"
)

// configPath is the checked-in budgets and allowlist; editing it regates everything.
const configPath = "tools/promptlint/promptlint.yaml"

type config struct {
	Budgets   map[string]int `yaml:"budgets"`
	Overrides map[string]int `yaml:"overrides"`
	Overlap   struct {
		Threshold float64      `yaml:"threshold"`
		Allow     []allowEntry `yaml:"allow"`
	} `yaml:"overlap"`
	ExternalSkills    []string     `yaml:"external_skills"`
	OrchestratorTools []string     `yaml:"orchestrator_tools"`
	Ignore            []ignoreRule `yaml:"ignore"`
}

// ignoreRule waives one known finding: same rule and subject, message containing Match.
type ignoreRule struct {
	Rule    string `yaml:"rule"`
	Subject string `yaml:"subject"`
	Match   string `yaml:"match"`
	Reason  string `yaml:"reason"`
}

// allowEntry permits an agent to be offered any two of Tools despite their overlap.
type allowEntry struct {
	Tools  []string `yaml:"tools"`
	Reason string   `yaml:"reason"`
}

func loadConfig(root string) (config, error) {
	var c config
	raw, err := os.ReadFile(filepath.Join(root, configPath))
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", configPath, err)
	}
	for _, a := range c.Overlap.Allow {
		if len(a.Tools) < 2 || strings.TrimSpace(a.Reason) == "" {
			return c, fmt.Errorf("%s: allow entry %v needs two or more tools and a reason", configPath, a.Tools)
		}
	}
	for _, r := range c.Ignore {
		if r.Rule == "" || r.Subject == "" || strings.TrimSpace(r.Reason) == "" {
			return c, fmt.Errorf("%s: ignore entry %+v needs rule, subject and reason", configPath, r)
		}
	}
	return c, nil
}

func (c config) budget(kind, id string) int {
	if b, ok := c.Overrides[id]; ok {
		return b
	}
	return c.Budgets[kind]
}

// allowIndex is the allowlist entry covering both a and b, or -1.
func (c config) allowIndex(a, b string) int {
	return slices.IndexFunc(c.Overlap.Allow, func(e allowEntry) bool {
		return slices.Contains(e.Tools, a) && slices.Contains(e.Tools, b)
	})
}

type finding struct {
	Rule    string `json:"rule"`
	Subject string `json:"subject"`
	Message string `json:"message"`
	Gated   bool   `json:"gated"`
	warn    bool
	sources []string
}

type report struct {
	Estimator string      `json:"estimator"`
	Items     []item      `json:"items"`
	Agents    []agentSpec `json:"agents"`
	Overlaps  []overlap   `json:"overlaps"`
	Findings  []finding   `json:"findings"`
}

// linter holds one run's inputs; changed is nil for a full check.
type linter struct {
	root    string
	cfg     config
	tools   map[string]toolDef
	changed []string
	rep     report
}

func run(root string, cfg config, defs []toolDef, changed []string) report {
	l := &linter{root: root, cfg: cfg, tools: map[string]toolDef{}, changed: changed}
	l.rep.Estimator = "bytes/4"
	for _, d := range defs {
		l.tools[d.Name] = d
	}
	l.measureFiles()
	l.measureTools()
	l.checkRefs()
	l.checkAgents()
	l.checkOverlaps(defs)
	l.gate()
	return l.rep
}

// add records f, demoted to a warning when an ignore entry waives it.
func (l *linter) add(f finding) {
	for _, r := range l.cfg.Ignore {
		if r.Rule == f.Rule && r.Subject == f.Subject && strings.Contains(f.Message, r.Match) {
			f.warn, f.Message = true, f.Message+" (ignored: "+r.Reason+")"
		}
	}
	l.rep.Findings = append(l.rep.Findings, f)
}

func (l *linter) addItem(id, kind string, size int, sources []string) {
	it := item{ID: id, Kind: kind, Tokens: tokens(size), Budget: l.cfg.budget(kind, id), Sources: sources}
	l.rep.Items = append(l.rep.Items, it)
	if it.Budget > 0 && it.Tokens > it.Budget {
		l.add(finding{Rule: "budget", Subject: id, sources: sources,
			Message: fmt.Sprintf("%d tokens over the %s budget of %d", it.Tokens, kind, it.Budget)})
	}
}

func (l *linter) measureFiles() {
	files := textFiles(l.root)
	for _, rel := range sortedKeys(files) {
		l.addItem(rel, files[rel], fileSize(l.root, rel), []string{rel})
	}
}

func (l *linter) measureTools() {
	for _, name := range sortedKeys(l.tools) {
		d := l.tools[name]
		l.addItem("tool:"+name, "tool", d.size(), toolSources(d))
	}
}

// toolSources: the Go tree that writes a tool's declaration; any change under it regates the tool.
func toolSources(d toolDef) []string {
	switch d.Provider {
	case "builtin", "core":
		return []string{"internal/tools/"}
	case skillToolset:
		return []string{"go.mod"}
	}
	return []string{"extensions/" + d.Provider + "/"}
}

// gate marks the findings a diff touches; warnings never gate, and a full check gates everything.
func (l *linter) gate() {
	for i, f := range l.rep.Findings {
		if f.warn {
			continue
		}
		touched := l.changed == nil || slices.Contains(l.changed, configPath) ||
			slices.ContainsFunc(l.changed, func(c string) bool { return hasPrefixAny(c, f.sources) })
		l.rep.Findings[i].Gated = touched
	}
}

// agents assembles every configured agent's offered surface.
func (l *linter) agents() []agentSpec {
	orch, cfgs, err := loadAgents(l.root)
	if err != nil {
		l.add(finding{Rule: "config", Subject: "config/quack.yaml", Message: err.Error(), sources: []string{"config/quack.yaml"}})
		return nil
	}
	orchTools := slices.Clone(l.cfg.OrchestratorTools)
	for _, name := range sortedKeys(l.tools) {
		if d := l.tools[name]; d.Provider == "core" && strings.HasPrefix(name, "write_") {
			orchTools = append(orchTools, name) // run.go offers every NewWriteKindTools kind
		}
	}
	orchTools = append(orchTools, deref(orch.Tools)...)
	out := []agentSpec{l.spec("orchestrator", orch, orchTools)}
	for _, name := range sortedKeys(cfgs) {
		c := cfgs[name]
		if c.dir == "" || c.ACP != nil {
			out = append(out, l.spec(name, c, nil))
			continue
		}
		out = append(out, l.spec(name, c, nativeTools(l.root, c)))
	}
	return out
}

// nativeTools mirrors serve's buildAgents: ResolveNames, the always-on artifact readers, the card's
// write tools, and the configured artifact tools.
func nativeTools(root string, c agentCfg) []string {
	configured := deref(c.Tools)
	out := append(tools.ResolveNames(configured, true), "list_artifacts", "read_artifact")
	if cardArtifact(root, c.dir) {
		out = append(out, "write_artifact", "edit_artifact")
	}
	for _, n := range configured {
		if tools.IsNativeArtifactTool(n) {
			out = append(out, n)
		}
	}
	return out
}

func (l *linter) spec(name string, c agentCfg, offered []string) agentSpec {
	a := agentSpec{Name: name, ACP: c.ACP != nil, Skills: deref(c.Skills), Budget: l.cfg.budget("agent", "agent:"+name)}
	if len(a.Skills) > 0 {
		offered = append(offered, "list_skills", "load_skill", "load_skill_resource")
	}
	a.Tools = uniq(offered)
	a.sources = []string{"config/quack.yaml"}
	if c.dir != "" {
		a.Prompts = bundlePrompts(l.root, c.dir)
		a.sources = append(a.sources, c.dir+"/agent-card.json", c.dir+"/agent.yaml")
		a.sources = append(a.sources, a.Prompts...)
	}
	for _, p := range a.Prompts {
		a.PromptTokens += tokens(fileSize(l.root, p))
	}
	for _, t := range a.Tools {
		if d, ok := l.tools[t]; ok {
			a.ToolTokens += tokens(d.size())
			a.sources = append(a.sources, toolSources(d)...)
		}
	}
	for _, s := range a.Skills {
		a.SkillTokens += tokens(len(frontmatter(l.root, s)))
		a.sources = append(a.sources, skillDir(l.root, s)+"/SKILL.md")
	}
	a.Total = a.PromptTokens + a.ToolTokens + a.SkillTokens
	return a
}

func (l *linter) checkAgents() {
	for _, a := range l.agents() {
		l.rep.Agents = append(l.rep.Agents, a)
		if a.Budget > 0 && a.Total > a.Budget {
			l.add(finding{Rule: "budget", Subject: "agent:" + a.Name, sources: a.sources,
				Message: fmt.Sprintf("system prompt + tools + skill list is %d tokens, over the agent budget of %d", a.Total, a.Budget)})
		}
		for _, t := range a.Tools {
			if _, ok := l.tools[t]; !ok {
				l.add(finding{Rule: "unknown_tool", Subject: "agent:" + a.Name, sources: a.sources,
					Message: fmt.Sprintf("offered tool %q, which no builtin, core tool or extension declares", t)})
			}
		}
		for _, s := range a.Skills {
			l.checkSkill("agent:"+a.Name, s, a.sources)
		}
	}
}
