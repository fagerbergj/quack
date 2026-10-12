package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// item is one measured text a model receives: a prompt, rubric, skill body, tool definition or agent total.
type item struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Tokens  int      `json:"tokens"`
	Budget  int      `json:"budget"`
	Sources []string `json:"-"`
}

// agentSpec is one agent's offered surface, assembled the way serve builds it.
type agentSpec struct {
	Name         string   `json:"name"`
	ACP          bool     `json:"acp,omitempty"`
	Prompts      []string `json:"prompts"`
	Tools        []string `json:"tools"`
	Skills       []string `json:"skills,omitempty"`
	PromptTokens int      `json:"prompt_tokens"`
	ToolTokens   int      `json:"tool_tokens"`
	SkillTokens  int      `json:"skill_list_tokens"`
	Total        int      `json:"total"`
	Budget       int      `json:"budget"`
	sources      []string
}

// agentCfg is the subset of a quack.yaml agents.<name> entry or a plugin agent.yaml this lint reads.
type agentCfg struct {
	Bundle string    `yaml:"bundle"`
	Tools  *[]string `yaml:"tools"`
	Skills *[]string `yaml:"skills"`
	ACP    any       `yaml:"acp"`
	dir    string
}

type quackYAML struct {
	Orchestrator agentCfg            `yaml:"orchestrator"`
	Agents       map[string]agentCfg `yaml:"agents"`
}

// tokens estimates model tokens as bytes/4, rounded up; no tokenizer is a dependency.
func tokens(n int) int { return (n + 3) / 4 }

// textFiles maps each measured repo file to its kind.
func textFiles(root string) map[string]string {
	out := map[string]string{}
	globs := map[string]string{
		"agents/*/prompt.md": "prompt", "agents/*/memory.md": "prompt", "agents/*/rubric.yaml": "rubric",
		"extensions/*/plugin/agents/*/prompt.md": "prompt", "extensions/*/plugin/agents/*/memory.md": "prompt",
		"extensions/*/plugin/agents/*/rubric.yaml": "rubric", "config/prompts/*.md": "prompt",
		"config/constitution.md": "prompt", "config/rubric.md": "rubric",
		"skills/*/SKILL.md": "skill", "extensions/*/plugin/skills/*/SKILL.md": "skill",
		".agents/embedded/dotagents/skills/*/SKILL.md": "skill",
	}
	for g, kind := range globs {
		matches, _ := filepath.Glob(filepath.Join(root, g))
		for _, m := range matches {
			rel, _ := filepath.Rel(root, m)
			out[filepath.ToSlash(rel)] = kind
		}
	}
	return out
}

func fileSize(root, rel string) int {
	st, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		return 0
	}
	return int(st.Size())
}

// loadAgents reads quack.yaml plus every plugin bundle's agent.yaml; a config entry overrides the
// plugin's fields one by one, as plugin seeding does.
func loadAgents(root string) (agentCfg, map[string]agentCfg, error) {
	var q quackYAML
	raw, err := os.ReadFile(filepath.Join(root, "config/quack.yaml"))
	if err != nil {
		return agentCfg{}, nil, err
	}
	if err := yaml.Unmarshal(raw, &q); err != nil {
		return agentCfg{}, nil, err
	}
	agents := map[string]agentCfg{}
	plugin, _ := filepath.Glob(filepath.Join(root, "extensions/*/plugin/agents/*/agent.yaml"))
	for _, p := range plugin {
		var a agentCfg
		if raw, err := os.ReadFile(p); err == nil && yaml.Unmarshal(raw, &a) == nil {
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			a.dir = filepath.ToSlash(rel)
			agents[filepath.Base(a.dir)] = a
		}
	}
	for name, c := range q.Agents {
		agents[name] = mergeAgent(agents[name], c)
	}
	q.Orchestrator.dir = "agents/orchestrator"
	return q.Orchestrator, agents, nil
}

func mergeAgent(base, over agentCfg) agentCfg {
	if over.Bundle != "" {
		base.dir = over.Bundle
	}
	if over.Tools != nil {
		base.Tools = over.Tools
	}
	if over.Skills != nil {
		base.Skills = over.Skills
	}
	if over.ACP != nil {
		base.ACP = over.ACP
	}
	return base
}

func deref(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// cardArtifact reads agent-card.json's artifact kind; a card that names one implies the write tools.
func cardArtifact(root, dir string) bool {
	raw, err := os.ReadFile(filepath.Join(root, dir, "agent-card.json"))
	if err != nil {
		return false
	}
	var c struct {
		Artifact string `json:"artifact"`
	}
	return json.Unmarshal(raw, &c) == nil && c.Artifact != ""
}

// bundlePrompts lists the prompt files a bundle's agent is sent as system prompt.
func bundlePrompts(root, dir string) []string {
	var out []string
	for _, f := range []string{"prompt.md", "memory.md"} {
		p := path.Join(dir, f)
		if fileSize(root, p) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hasPrefixAny reports whether p equals or sits under any of prefixes (directory prefixes end in /).
func hasPrefixAny(p string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(pre string) bool {
		return p == pre || (strings.HasSuffix(pre, "/") && strings.HasPrefix(p, pre))
	})
}
