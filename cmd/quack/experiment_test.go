package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/config"
)

func TestRunExperimentRun_MissingConfigDegrades(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	cmd := &cobra.Command{}
	if err := runExperimentRun(cmd, "my-dataset", "code-reviewer", "", "", 0, false); err == nil {
		t.Fatal("want an error when there is no local quack.yaml")
	}
}

func TestNowStamp_Format(t *testing.T) {
	if got := nowStamp(); len(got) != len("20060102-150405") {
		t.Fatalf("nowStamp() = %q, want the fixed-width 20060102-150405 layout", got)
	}
}

// TestPinnedPromptSource_NameMustMatchAgent: a pin whose name isn't "system/"+agent fails loudly
// instead of silently resolving the agent's unpinned prompt.
func TestPinnedPromptSource_NameMustMatchAgent(t *testing.T) {
	_, err := pinnedPromptSource(context.Background(), &config.Config{}, "code-reviewer", "system/synthesizer@5")
	if err == nil || !strings.Contains(err.Error(), "system/code-reviewer") {
		t.Fatalf("err = %v, want a mismatch error naming system/code-reviewer", err)
	}
}

// A plugin agent's rubric scales resolve from its local clone; an agent nowhere in the config is an error.
func TestExperimentRubric_PluginAgent(t *testing.T) {
	cfgPath := writeScoutPluginConfig(t)
	r, err := experimentRubric(context.Background(), cfgPath, "scout")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Score("cites_sources", 0.5); got != 0.5 {
		t.Errorf("cites_sources = %v, want 0.5 on its declared 0-1 scale", got)
	}
	if got := r.Score("grounded", 0.5); got != 1.5 {
		t.Errorf("grounded = %v, want 1.5 on the default 0-3 scale", got)
	}
	if _, err := experimentRubric(context.Background(), cfgPath, "nobody"); err == nil {
		t.Fatal("want an error for an agent neither quack.yaml nor a plugin defines")
	}
}

func TestNewExperimentCmd_Tree(t *testing.T) {
	c := newExperimentCmd()
	run, _, err := c.Find([]string{"run"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if run.Use != "run" {
		t.Fatalf("Use = %q", run.Use)
	}
}

// prompt push uploads the bundle's prompt.md as a new system/<agent> version with no production label.
func TestRunPromptPush_UnlabelledVersionFromBundle(t *testing.T) {
	var posted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/public/v2/prompts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		_, _ = w.Write([]byte(`{"name":"system/scout","version":4,"type":"text","prompt":"x"}`))
	}))
	defer srv.Close()

	cfgPath := writeScoutPluginConfig(t)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(raw), "stores:\n", "stores:\n  lf: {kind: langfuse, url: "+srv.URL+", public_key: pk, secret_key: sk}\n", 1) + "prompts:\n  store: lf\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfgPath), "acme", "agents", "scout", "prompt.md"), []byte("SCOUT PROMPT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)

	var out bytes.Buffer
	c := newPromptCmd()
	c.SetOut(&out)
	c.SetArgs([]string{"push", "scout"})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted["name"] != "system/scout" || posted["prompt"] != "SCOUT PROMPT\n" {
		t.Fatalf("posted = %+v, want the bundle prompt.md as system/scout", posted)
	}
	if labels, ok := posted["labels"].([]any); !ok || len(labels) != 0 {
		t.Fatalf("labels = %#v, want none: a pushed baseline must not take production", posted["labels"])
	}
	if out.String() != "system/scout@4\n" {
		t.Fatalf("out = %q, want the new version to pin", out.String())
	}
}
