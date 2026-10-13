package main

import (
	"context"
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
