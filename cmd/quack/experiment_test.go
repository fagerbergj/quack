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
