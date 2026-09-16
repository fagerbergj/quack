package serve

import (
	"testing"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
)

// A Langfuse edit to system/compaction must reach the summarizer, so the built
// Compaction has to carry the resolver (it was dropped once and only the docs noticed).
func TestBuildCompactionCarriesResolver(t *testing.T) {
	cfg := &config.Config{}
	cfg.Session.Compaction.Enabled = true
	res := artifactsrc.New("", nil, 0)
	compactionFor, err := buildCompaction(cfg, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	comp := compactionFor(config.AgentConfig{ContextWindow: 1000}, nil)
	if comp.Prompts != res {
		t.Fatalf("Compaction.Prompts = %v, want the boot resolver", comp.Prompts)
	}
}
