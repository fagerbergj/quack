package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// judgeModelEnv joins researcherModelEnv/coderModelFallbackEnv as a
// model_role target (see modelRoleEnv).
const judgeModelEnv = "QUACK_JUDGE_MODEL"

// modelRoleEnv substitutes for the "default model" concept config doesn't
// have: a plugin agent names a role, not a served model id.
var modelRoleEnv = map[string]string{
	"researcher": researcherModelEnv,
	"coder":      coderModelFallbackEnv,
	"judge":      judgeModelEnv,
}

// pluginAgentDefaultsFile is the plugin-bundle-only sibling of
// agent-card.json/prompt.md (docs/configuration/agents.md).
const pluginAgentDefaultsFile = "agent.yaml"

// pluginAgentDefaults is agent.yaml's shape - what a deployment's
// agents.<name>: entry can override field by field.
type pluginAgentDefaults struct {
	Tools         []string `yaml:"tools"`
	Skills        []string `yaml:"skills"`
	JudgeRounds   int      `yaml:"judge_rounds"`
	ContextWindow int      `yaml:"context_window"`
	// ModelRole: see modelRoleEnv.
	ModelRole string `yaml:"model_role"`
}

func readPluginAgentDefaults(bundleDir string) (pluginAgentDefaults, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, pluginAgentDefaultsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return pluginAgentDefaults{}, nil
		}
		return pluginAgentDefaults{}, err
	}
	var d pluginAgentDefaults
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return pluginAgentDefaults{}, fmt.Errorf("parse %s: %w", pluginAgentDefaultsFile, err)
	}
	if d.ModelRole != "" {
		if _, ok := modelRoleEnv[d.ModelRole]; !ok {
			return pluginAgentDefaults{}, fmt.Errorf("%s: model_role %q must be one of researcher, coder, judge", pluginAgentDefaultsFile, d.ModelRole)
		}
	}
	return d, nil
}

// mergeAgentConfig applies override's non-zero fields onto base, field by
// field. Bundle and Optional stay plugin-owned regardless of override.
func mergeAgentConfig(base, override AgentConfig) AgentConfig {
	merged := base
	if override.Provider != "" {
		merged.Provider = override.Provider
	}
	if override.Model != "" {
		merged.Model = override.Model
	}
	if override.ContextWindow != 0 {
		merged.ContextWindow = override.ContextWindow
	}
	if len(override.Tools) > 0 {
		merged.Tools = override.Tools
	}
	if len(override.Inputs) > 0 {
		merged.Inputs = override.Inputs
	}
	if override.Gated != nil {
		merged.Gated = override.Gated
	}
	if override.JudgeRounds != 0 {
		merged.JudgeRounds = override.JudgeRounds
	}
	if override.Judge != nil {
		merged.Judge = override.Judge
	}
	if override.Memory.Bucket != "" {
		merged.Memory = override.Memory
	}
	if len(override.Skills) > 0 {
		merged.Skills = override.Skills
	}
	if override.Acp != nil {
		merged.Acp = override.Acp
	}
	merged.Bundle = base.Bundle
	merged.Optional = true
	return merged
}

// SeedPluginAgents merges one plugin's agents/<bundle>/ directories into
// c.Agents; an existing entry (a deployment override) wins field by field
// via mergeAgentConfig. Re-validates with validateAgentModel.
func (c *Config) SeedPluginAgents(pluginName, agentsDir string) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: read agents dir: %w", pluginName, err)
	}
	if c.Agents == nil {
		c.Agents = map[string]AgentConfig{}
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		bundleDir := filepath.Join(agentsDir, name)
		if _, err := os.Stat(filepath.Join(bundleDir, "agent-card.json")); err != nil {
			continue
		}
		defaults, err := readPluginAgentDefaults(bundleDir)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: agent %q: %w", pluginName, name, err)
		}
		base := AgentConfig{
			Bundle:        bundleDir,
			Provider:      "default",
			Model:         expandEnv(modelRoleEnv[defaults.ModelRole]),
			ContextWindow: defaults.ContextWindow,
			Tools:         defaults.Tools,
			Skills:        defaults.Skills,
			JudgeRounds:   defaults.JudgeRounds,
			Optional:      true,
		}
		seeded := base
		if override, ok := c.Agents[name]; ok {
			seeded = mergeAgentConfig(base, override)
		}
		c.Agents[name] = seeded
		names = append(names, name)
	}
	sort.Strings(names)
	if err := c.validateAgentModel(); err != nil {
		return nil, fmt.Errorf("plugin %q: %w", pluginName, err)
	}
	return names, nil
}

// SeedPluginShapes merges one plugin's workflows/*.yaml files into
// c.Workflows, re-validated by validateWorkflows. A shape whose Name already
// exists (a config entry, or an earlier plugin's) is skipped, not duplicated.
func (c *Config) SeedPluginShapes(pluginName, workflowsDir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(workflowsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("plugin %q: read workflows dir: %w", pluginName, err)
	}
	sort.Strings(files)
	existing := make(map[string]bool, len(c.Workflows))
	for _, w := range c.Workflows {
		existing[w.Name] = true
	}
	var attempted []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: workflow %q: %w", pluginName, filepath.Base(f), err)
		}
		var shape WorkflowShape
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&shape); err != nil {
			return nil, fmt.Errorf("plugin %q: workflow %q: %w", pluginName, filepath.Base(f), err)
		}
		if existing[shape.Name] {
			continue
		}
		c.Workflows = append(c.Workflows, shape)
		existing[shape.Name] = true
		attempted = append(attempted, shape.Name)
	}
	if err := c.validateWorkflows(); err != nil {
		return nil, fmt.Errorf("plugin %q: %w", pluginName, err)
	}
	present := make(map[string]bool, len(c.Workflows))
	for _, w := range c.Workflows {
		present[w.Name] = true
	}
	seeded := attempted[:0] // in-place filter: validateWorkflows may have dropped some
	for _, n := range attempted {
		if present[n] {
			seeded = append(seeded, n)
		}
	}
	return seeded, nil
}
