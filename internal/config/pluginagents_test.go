package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseConfigForPluginSeed loads a minimal, otherwise-valid Config with one
// registered agent (host) and one model (m) - the fixture every
// SeedPluginAgents/SeedPluginShapes test merges into.
func baseConfigForPluginSeed(t *testing.T) *Config {
	t.Helper()
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")
	path := writeTemp(t, `
providers:
  default: { kind: openai, endpoint: http://localhost:1 }
models:
  m: { provider: default, role: worker }
stores:
  main: { kind: sqlite, url: /tmp/x.db }
session: { store: main }
orchestrator: { provider: default, model: m }
agents:
  host:
    bundle: agents/host
    provider: default
    model: m
workspace:
  root: `+t.TempDir()+`
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func writeAgentBundle(t *testing.T, agentsDir, name, agentYAML string) {
	t.Helper()
	dir := filepath.Join(agentsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-card.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if agentYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(agentYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeedPluginAgents_ModelRoleAndDefaults(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "model_role: researcher\ntools: [web_search]\ncontext_window: 4096\njudge_rounds: 2\n")

	names, err := c.SeedPluginAgents("acme", agentsDir)
	if err != nil {
		t.Fatalf("SeedPluginAgents: %v", err)
	}
	if len(names) != 1 || names[0] != "scout" {
		t.Fatalf("names = %v, want [scout]", names)
	}
	ac := c.Agents["scout"]
	if ac.Model != "m" || ac.Provider != "default" {
		t.Errorf("model/provider = %q/%q, want m/default", ac.Model, ac.Provider)
	}
	if !ac.Optional {
		t.Error("plugin agent must be implicitly optional")
	}
	if len(ac.Tools) != 1 || ac.Tools[0] != "web_search" {
		t.Errorf("tools = %v, want [web_search]", ac.Tools)
	}
	if ac.ContextWindow != 4096 || ac.JudgeRounds != 2 {
		t.Errorf("context_window/judge_rounds = %d/%d, want 4096/2", ac.ContextWindow, ac.JudgeRounds)
	}
	if ac.Bundle != filepath.Join(agentsDir, "scout") {
		t.Errorf("bundle = %q, want %q", ac.Bundle, filepath.Join(agentsDir, "scout"))
	}
}

// A deployment's agents.<name>: entry, already in c.Agents before seeding,
// overrides the plugin's defaults field by field - untouched fields keep
// the plugin's value, and Bundle/Optional stay plugin-owned regardless.
func TestSeedPluginAgents_DeploymentOverrideWinsFieldByField(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	t.Setenv("QUACK_JUDGE_MODEL", "judge-model")
	c.Models["judge-model"] = ModelConfig{Provider: "default", Role: "judge"}
	c.Agents["scout"] = AgentConfig{Model: "judge-model", ContextWindow: 8192, Optional: false, Bundle: "should-be-ignored"}

	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "model_role: researcher\ntools: [web_search]\njudge_rounds: 3\n")

	if _, err := c.SeedPluginAgents("acme", agentsDir); err != nil {
		t.Fatalf("SeedPluginAgents: %v", err)
	}
	ac := c.Agents["scout"]
	if ac.Model != "judge-model" {
		t.Errorf("model = %q, want override judge-model", ac.Model)
	}
	if ac.ContextWindow != 8192 {
		t.Errorf("context_window = %d, want override 8192", ac.ContextWindow)
	}
	if ac.JudgeRounds != 3 {
		t.Errorf("judge_rounds = %d, want plugin default 3 (override left it unset)", ac.JudgeRounds)
	}
	if !ac.Optional {
		t.Error("Optional must stay true even though the override set it false")
	}
	if ac.Bundle != filepath.Join(agentsDir, "scout") {
		t.Errorf("bundle = %q, want the plugin bundle dir, not the override's", ac.Bundle)
	}
}

func TestSeedPluginAgents_UnknownModelRoleErrorsNamingPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "model_role: wizard\n")

	_, err := c.SeedPluginAgents("acme", agentsDir)
	if err == nil {
		t.Fatal("expected an error for an unknown model_role")
	}
	if !strings.Contains(err.Error(), "acme") {
		t.Errorf("error %q must name the plugin", err.Error())
	}
}

func TestSeedPluginShapes_AppearsAndValidates(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := t.TempDir()
	shape := "name: acme-job\ntrigger: \"Run the acme job\"\nagents: [host]\nshape: \"ONE `host` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := c.SeedPluginShapes("acme", workflowsDir)
	if err != nil {
		t.Fatalf("SeedPluginShapes: %v", err)
	}
	if len(names) != 1 || names[0] != "acme-job" {
		t.Fatalf("names = %v, want [acme-job]", names)
	}
	if len(c.Workflows) != 1 || c.Workflows[0].Name != "acme-job" {
		t.Fatalf("c.Workflows = %+v, want one acme-job entry", c.Workflows)
	}
}

// A non-atomic deploy's exact failure mode: the old config still carries a
// workflows: entry the new plugin also ships. The config's entry must win -
// one row, no duplicate, and (since compose()'s trigger-collision warning
// only fires on a genuine second row) nothing left for it to collide with.
func TestSeedPluginShapes_DedupesAgainstExistingConfigShape(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	c.Workflows = append(c.Workflows, WorkflowShape{
		Name: "acme-job", Trigger: "Run the OLD acme job", Agents: []string{"host"}, Shape: "ONE `host` node",
	})
	workflowsDir := t.TempDir()
	shape := "name: acme-job\ntrigger: \"Run the NEW acme job\"\nagents: [host]\nshape: \"ONE `host` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := c.SeedPluginShapes("acme", workflowsDir)
	if err != nil {
		t.Fatalf("SeedPluginShapes: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none (the config's shape already covers this name)", names)
	}
	if len(c.Workflows) != 1 {
		t.Fatalf("c.Workflows = %+v, want exactly one row, no duplicate", c.Workflows)
	}
	if c.Workflows[0].Trigger != "Run the OLD acme job" {
		t.Errorf("c.Workflows[0].Trigger = %q, want the config's entry to win over the plugin's", c.Workflows[0].Trigger)
	}
}

// Two plugins declaring the same shape name: the first to seed wins,
// matching the "first in seed order wins" rule bare-skill-name resolution
// already uses.
func TestSeedPluginShapes_DedupesAgainstEarlierPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	firstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstDir, "acme-job.yaml"), []byte("name: acme-job\ntrigger: \"first\"\nagents: [host]\nshape: \"ONE `host` node\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secondDir, "acme-job.yaml"), []byte("name: acme-job\ntrigger: \"second\"\nagents: [host]\nshape: \"ONE `host` node\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := c.SeedPluginShapes("first-plugin", firstDir); err != nil {
		t.Fatalf("SeedPluginShapes(first): %v", err)
	}
	names, err := c.SeedPluginShapes("second-plugin", secondDir)
	if err != nil {
		t.Fatalf("SeedPluginShapes(second): %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none (first-plugin already seeded acme-job)", names)
	}
	if len(c.Workflows) != 1 || c.Workflows[0].Trigger != "first" {
		t.Fatalf("c.Workflows = %+v, want exactly the first plugin's entry", c.Workflows)
	}
}

// A shape naming an agent nobody configured must fail loud (never silently
// dropped) and name the plugin, so a bad bundle points straight at its source.
func TestSeedPluginShapes_MissingAgentFailsNamingPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := t.TempDir()
	shape := "name: acme-job\ntrigger: \"Run the acme job\"\nagents: [ghost]\nshape: \"ONE `ghost` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := c.SeedPluginShapes("acme", workflowsDir)
	if err == nil {
		t.Fatal("expected an error for a shape naming an unconfigured agent")
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q must name both the plugin and the missing agent", err.Error())
	}
}

// A structurally incomplete shape is dropped with validateWorkflows' own
// warning, same as a config-authored one - not a boot error, and absent from
// the returned names.
func TestSeedPluginShapes_MalformedShapeDroppedNotFatal(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := t.TempDir()
	shape := "name: acme-job\ntrigger: \"\"\nagents: [host]\nshape: \"ONE `host` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := c.SeedPluginShapes("acme", workflowsDir)
	if err != nil {
		t.Fatalf("SeedPluginShapes: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none (malformed shape dropped)", names)
	}
}

// A bundle with no agent.yaml gets zero defaults, not an error - agent.yaml
// is optional, same as a shipped bundle's rubric.yaml/memory.md.
func TestSeedPluginAgents_NoAgentYAMLUsesZeroDefaults(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	c.skipRuntimeValidation = true // no model_role, no override - skip the empty-model check
	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "")

	if _, err := c.SeedPluginAgents("acme", agentsDir); err != nil {
		t.Fatalf("SeedPluginAgents: %v", err)
	}
	ac := c.Agents["scout"]
	if ac.Model != "" || len(ac.Tools) != 0 {
		t.Errorf("scout = %+v, want zero defaults with no agent.yaml", ac)
	}
}

// agent.yaml existing but unreadable as a regular file (bundleDir is itself
// a file, so the join can't be a directory) surfaces the raw read error, not
// the NotExist short-circuit.
// "scout" is a FILE, not a directory: agent.yaml can't be a child path
// under it, so os.ReadFile fails with a non-NotExist error.
func TestReadPluginAgentDefaults_NonNotExistReadErrorPropagates(t *testing.T) {
	scoutPath := filepath.Join(t.TempDir(), "scout")
	if err := os.WriteFile(scoutPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readPluginAgentDefaults(scoutPath)
	if err == nil || os.IsNotExist(err) {
		t.Fatalf("readPluginAgentDefaults(file-as-dir) = %v, want a non-NotExist error", err)
	}
}

func TestReadPluginAgentDefaults_MalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("tools: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPluginAgentDefaults(dir); err == nil {
		t.Fatal("expected a parse error for malformed agent.yaml")
	}
}

// mergeAgentConfig: every overridable field wins when the override sets it -
// the field-by-field contract, one field at a time isn't enough to prove it.
func TestMergeAgentConfig_EveryFieldOverrides(t *testing.T) {
	base := AgentConfig{Bundle: "plugin/bundle", Provider: "default", Optional: true}
	trueVal := true
	override := AgentConfig{
		Provider: "custom", Model: "m2", ContextWindow: 999,
		Tools: []string{"t1"}, Inputs: []string{"text"}, Gated: &trueVal, JudgeRounds: 5, Judge: &trueVal,
		Memory: MemoryConfig{Bucket: "coding"}, Skills: []string{"s1"}, Acp: &AcpAgentConfig{Command: []string{"pi"}},
		Bundle: "ignored", Optional: false,
	}
	merged := mergeAgentConfig(base, override)
	if merged.Provider != "custom" || merged.Model != "m2" || merged.ContextWindow != 999 {
		t.Errorf("merged = %+v, want provider/model/context_window overridden", merged)
	}
	if len(merged.Tools) != 1 || len(merged.Inputs) != 1 || merged.Gated == nil || merged.JudgeRounds != 5 || merged.Judge == nil {
		t.Errorf("merged = %+v, want tools/inputs/gated/judge_rounds/judge overridden", merged)
	}
	if merged.Memory.Bucket != "coding" || len(merged.Skills) != 1 || merged.Acp == nil {
		t.Errorf("merged = %+v, want memory/skills/acp overridden", merged)
	}
	if merged.Bundle != "plugin/bundle" || !merged.Optional {
		t.Errorf("merged = %+v, want Bundle/Optional to stay plugin-owned", merged)
	}
}

func TestSeedPluginAgents_ReadDirErrorNamesPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	_, err := c.SeedPluginAgents("acme", filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("SeedPluginAgents(missing dir) = %v, want an error naming the plugin", err)
	}
}

// A nil c.Agents (a Config built directly, not through Load) still seeds -
// the lazy-init path config-authored callers never hit since Load's decode
// always leaves a non-nil map when agents: is present.
func TestSeedPluginAgents_NilAgentsMapInitializes(t *testing.T) {
	c := &Config{
		Providers: map[string]ProviderConfig{"default": {Kind: "openai", Endpoint: "http://x"}},
		Models:    map[string]ModelConfig{"m": {Provider: "default", Role: "worker"}},
	}
	c.skipRuntimeValidation = true // nil Agents map, no override to supply a model - skip the empty-model check
	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "")

	if _, err := c.SeedPluginAgents("acme", agentsDir); err != nil {
		t.Fatalf("SeedPluginAgents with nil c.Agents: %v", err)
	}
	if _, ok := c.Agents["scout"]; !ok {
		t.Fatal("scout not seeded into a nil c.Agents map")
	}
}

// A stray non-directory entry and a directory missing agent-card.json are
// both skipped, not treated as bundles.
func TestSeedPluginAgents_SkipsNonDirAndCardlessEntries(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	c.skipRuntimeValidation = true // no model_role, no override - skip the empty-model check
	agentsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentsDir, "README.md"), []byte("not a bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentsDir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentBundle(t, agentsDir, "scout", "")

	names, err := c.SeedPluginAgents("acme", agentsDir)
	if err != nil {
		t.Fatalf("SeedPluginAgents: %v", err)
	}
	if len(names) != 1 || names[0] != "scout" {
		t.Fatalf("names = %v, want only [scout] (stray file and cardless dir skipped)", names)
	}
}

// A plugin agent whose model_role resolves to a model id absent from
// models: fails the same validateAgentModel check a config-authored agent
// naming an unregistered model would.
func TestSeedPluginAgents_ValidateAgentModelErrorNamesPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	t.Setenv("QUACK_RESEARCHER_MODEL", "not-a-registered-model")
	agentsDir := t.TempDir()
	writeAgentBundle(t, agentsDir, "scout", "model_role: researcher\n")

	_, err := c.SeedPluginAgents("acme", agentsDir)
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("SeedPluginAgents = %v, want an error naming the plugin", err)
	}
}

// filepath.Glob rejects a malformed pattern (unterminated character class)
// rather than silently matching nothing.
func TestSeedPluginShapes_GlobBadPatternErrorsNamingPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := filepath.Join(t.TempDir(), "bad[dir")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := c.SeedPluginShapes("acme", workflowsDir)
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("SeedPluginShapes(bad glob pattern) = %v, want an error naming the plugin", err)
	}
}

// A *.yaml glob match that's actually a directory fails the read, not the
// decode - os.ReadFile on a directory always errors.
func TestSeedPluginShapes_ReadFileErrorNamesPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workflowsDir, "acme-job.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := c.SeedPluginShapes("acme", workflowsDir)
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("SeedPluginShapes(dir named *.yaml) = %v, want an error naming the plugin", err)
	}
}

func TestSeedPluginShapes_MalformedYAMLDecodeErrorNamesPlugin(t *testing.T) {
	c := baseConfigForPluginSeed(t)
	workflowsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte("name: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := c.SeedPluginShapes("acme", workflowsDir)
	if err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("SeedPluginShapes(malformed yaml) = %v, want an error naming the plugin", err)
	}
}
