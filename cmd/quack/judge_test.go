package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/ledger"
)

// TestNewJudgeReplayCmd_Flags checks the command tree and flag wiring; the
// RunE closure itself is exercised end-to-end below.
func TestNewJudgeReplayCmd_Flags(t *testing.T) {
	c := newJudgeCmd()
	replay, _, err := c.Find([]string{"replay"})
	if err != nil {
		t.Fatalf("find replay subcommand: %v", err)
	}
	for _, name := range []string{"node", "round", "rubric", "from-server", "deterministic-only", "repeat", "out", "json"} {
		if replay.Flags().Lookup(name) == nil {
			t.Errorf("replay command missing --%s flag", name)
		}
	}
}

// TestRunJudgeReplay_NoConfig: no local quack.yaml at QUACK_CONFIG is a
// clear error naming `quack init`, not a panic or a generic failure.
func TestRunJudgeReplay_NoConfig(t *testing.T) {
	t.Setenv("QUACK_CONFIG", filepath.Join(t.TempDir(), "missing-quack.yaml"))
	bundlePath := filepath.Join(t.TempDir(), "bundle.jsonl")
	if err := os.WriteFile(bundlePath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newJudgeReplayCmd()
	c.SetContext(context.Background())
	if err := runJudgeReplay(c, bundlePath, cli.ReplayOptions{Repeat: 1, DeterministicOnly: true}, "", false, false); err == nil || !strings.Contains(err.Error(), "quack init") {
		t.Fatal("runJudgeReplay with no quack.yaml: err = nil, want an error naming `quack init`")
	}
}

// TestRunJudgeReplay_DeterministicOnlyEndToEnd drives the real command
// handler against a local bundle file and an on-disk quack.yaml + rubric.yaml.
func TestRunJudgeReplay_DeterministicOnlyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	rubricPath := filepath.Join(dir, "rubric.yaml")
	rubricDoc := `scale: {min: 0, max: 3, pass: 2}
criteria:
  cites_sources:
    definition: existence check
    deterministic: true
    fix: fetch it
    scale: {min: 0, max: 1, pass: 0.85}
`
	if err := os.WriteFile(rubricPath, []byte(rubricDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	// The shipped `quack init` template - the smallest config guaranteed to
	// pass full validation (providers, models, session store, agent roster).
	t.Setenv("QUACK_CONFIG", "../../config/quack.yaml")
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://localhost:1"}, {"QUACK_LLM_API_KEY", "x"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"},
		{"QUACK_JUDGE_MODEL", "qwen3.8-27b"}, {"QUACK_EMBED_MODEL", "qwen3.8-27b"},
		{"QUACK_IMAGE_MODEL", "qwen3-vl-32b"}, {"QUACK_MEDIA_MODEL", "qwen3.8-27b"},
		{"QUACK_COMPACTION_MODEL", "qwen3.8-27b"}, {"QUACK_DATABASE_URL", "sqlite://" + filepath.Join(dir, "t.db")},
		{"QUACK_WORKSPACE_ROOT", dir},
	} {
		t.Setenv(kv[0], kv[1])
	}

	bundlePath := writeJudgeFixtureBundle(t, "web-researcher")
	c := newJudgeReplayCmd()
	c.SetContext(context.Background())
	if err := runJudgeReplay(c, bundlePath, cli.ReplayOptions{Repeat: 1, RubricPath: rubricPath, DeterministicOnly: true}, "", false, false); err != nil {
		t.Fatalf("runJudgeReplay: %v", err)
	}
}

// A plugin-seeded agent's bundled rubric resolves from the plugin's local clone, with no --rubric.
func TestRunJudgeReplay_PluginAgentBundledRubric(t *testing.T) {
	dir := t.TempDir()
	pluginDir := writeAcmeScoutPlugin(t, dir)
	rubric := "scale: {min: 0, max: 3, pass: 2}\ncriteria:\n  cites_sources:\n    definition: existence check\n" +
		"    deterministic: true\n    fix: fetch it\n    scale: {min: 0, max: 1, pass: 0.85}\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "agents", "scout", "rubric.yaml"), []byte(rubric), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "providers:\n  default:\n    kind: openai\n    endpoint: http://localhost:1\n    api_key: x\n" +
		"orchestrator:\n  provider: default\n  model: m\nmodels:\n  m:\n    provider: default\n    role: worker\n" +
		"stores:\n  default:\n    kind: sqlite\n    url: " + filepath.Join(dir, "store.db") + "\n" +
		"session:\n  store: default\nworkspace:\n  root: " + filepath.Join(dir, "workspace") + "\n" +
		"plugins:\n  seed:\n    - " + pluginDir + "\n"
	cfgPath := filepath.Join(dir, "quack.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)
	t.Setenv("QUACK_RESEARCHER_MODEL", "m")

	c := newJudgeReplayCmd()
	c.SetContext(context.Background())
	if err := runJudgeReplay(c, writeJudgeFixtureBundle(t, "scout"), cli.ReplayOptions{Repeat: 1, DeterministicOnly: true}, "", false, false); err != nil {
		t.Fatalf("runJudgeReplay: %v", err)
	}
}

func writeJudgeFixtureBundle(t *testing.T, agent string) string {
	t.Helper()
	now := time.Now()
	fetchCall := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "1", Name: "web_fetch"}}}}
	fetchResp := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "web_fetch",
		Response: map[string]any{"results": []any{map[string]any{"url": "https://example.com/a"}}}}}}}
	answer := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "See [source](https://example.com/a)."}}}
	input, err := json.Marshal([]*genai.Content{fetchCall, fetchResp})
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(ledger.LLMCallPayload{Input: string(input), Output: string(output)})
	if err != nil {
		t.Fatal(err)
	}
	scorePayload, err := json.Marshal(ledger.EvalScorePayload{Criterion: "cites_sources", Score: 1.0})
	if err != nil {
		t.Fatal(err)
	}
	entries := []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", NodeID: "node-1", Agent: agent, Round: "worker-r0", Kind: ledger.KindLLMCall, At: now, Payload: payload},
		{Seq: 2, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(time.Second), Payload: scorePayload},
	}
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(b)
		f.Write([]byte("\n"))
	}
	return path
}

// score-golden --deterministic-only needs no quack.yaml and writes the --json report to --out.
func TestScoreGolden_DeterministicOnlyWritesReport(t *testing.T) {
	t.Setenv("QUACK_CONFIG", filepath.Join(t.TempDir(), "missing-quack.yaml"))
	out := filepath.Join(t.TempDir(), "golden.json")
	c := newJudgeScoreGoldenCmd()
	c.SetOut(&strings.Builder{})
	c.SetArgs([]string{"../../testdata/judge-golden", "--deterministic-only", "--json", "--out", out})
	if err := c.Execute(); err != nil {
		t.Fatalf("score-golden: %v", err)
	}
	var rep cli.GoldenReport
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Live || rep.Overall.Labeled == 0 {
		t.Fatalf("report = %+v (err %v), want an offline report with scored labels", rep, err)
	}
}

// Without --deterministic-only, a missing quack.yaml is an error naming the way out.
func TestScoreGolden_LiveNeedsAConfiguredJudge(t *testing.T) {
	t.Setenv("QUACK_CONFIG", filepath.Join(t.TempDir(), "missing-quack.yaml"))
	c := newJudgeScoreGoldenCmd()
	c.SetArgs([]string{"../../testdata/judge-golden"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--deterministic-only") {
		t.Fatalf("err = %v, want one naming --deterministic-only", err)
	}

	// The shipped config with QUACK_JUDGE_MODEL unset loads but has the judge disabled.
	dir := t.TempDir()
	t.Setenv("QUACK_CONFIG", "../../config/quack.yaml")
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://localhost:1"}, {"QUACK_LLM_API_KEY", "x"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"},
		{"QUACK_JUDGE_MODEL", ""}, {"QUACK_EMBED_MODEL", "qwen3.8-27b"},
		{"QUACK_IMAGE_MODEL", "qwen3-vl-32b"}, {"QUACK_MEDIA_MODEL", "qwen3.8-27b"},
		{"QUACK_COMPACTION_MODEL", "qwen3.8-27b"}, {"QUACK_DATABASE_URL", "sqlite://" + filepath.Join(dir, "t.db")},
		{"QUACK_WORKSPACE_ROOT", dir},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c = newJudgeScoreGoldenCmd()
	c.SetArgs([]string{"../../testdata/judge-golden"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "gates.judge is not configured") {
		t.Fatalf("err = %v, want the judge-not-configured error", err)
	}
}
