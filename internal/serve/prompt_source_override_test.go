package serve

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
)

type stubSource struct{}

func (stubSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{}, false, nil
}
func (stubSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// An experiment's pinned Source must win over the configured store and a replay.
func TestPromptSourceForPrefersOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Prompts.Store = "lf"
	got, name, err := promptSourceFor(context.Background(), cfg, stubSource{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(stubSource); !ok || name != "lf" {
		t.Fatalf("got %T %q, want stubSource \"lf\"", got, name)
	}
	got, _, err = promptSourceFor(context.Background(), &config.Config{}, nil)
	if err != nil || got != nil {
		t.Fatalf("no override, no store: got %v %v", got, err)
	}
}
