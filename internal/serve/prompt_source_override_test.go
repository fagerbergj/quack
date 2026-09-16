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

// An experiment's pinned Source chains ahead of the configured store, not in place of
// it (#1424 item 6): a name the override doesn't pin still resolves off the store.
func TestPromptSourceForChainsOverrideBeforeStore(t *testing.T) {
	cfg := &config.Config{}
	cfg.Prompts.Store = "lf"
	got, name := promptSourceFor(cfg, stubSource{})
	chain, ok := got.(*artifactsrc.ChainSource)
	if !ok || name != "lf" {
		t.Fatalf("got %T %q, want *ChainSource \"lf\"", got, name)
	}
	if len(chain.Sources) != 2 {
		t.Fatalf("want [override, store], got %d sources", len(chain.Sources))
	}
	if _, ok := chain.Sources[0].(stubSource); !ok {
		t.Fatalf("chain.Sources[0] = %T, want the override first", chain.Sources[0])
	}

	got, _ = promptSourceFor(&config.Config{}, nil)
	if got != nil {
		t.Fatalf("no override, no store: got %v", got)
	}
}
