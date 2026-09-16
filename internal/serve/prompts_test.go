package serve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
)

func TestBuildPromptSourceNoStore(t *testing.T) {
	src, name := buildPromptSource(&config.Config{})
	if src != nil || name != "" {
		t.Fatalf("src=%v name=%q, want nil, \"\" with no prompts.store configured", src, name)
	}
}

func TestBuildPromptSourceLangfuse(t *testing.T) {
	cfg := &config.Config{
		Prompts: config.PromptsConfig{Store: "lf"},
		Stores: map[string]config.StoreConfig{
			"lf": {Kind: "langfuse", URL: "http://x", PublicKey: "pub", SecretKey: "sec"},
		},
	}
	src, name := buildPromptSource(cfg)
	if src == nil || name != "lf" {
		t.Fatalf("src=%v name=%q, want a non-nil Source and \"lf\"", src, name)
	}
}

type stubPromptSource struct {
	mu     sync.Mutex
	seeded []string
	err    error
}

func (s *stubPromptSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{}, false, nil
}

func (s *stubPromptSource) Seed(_ context.Context, name string, _ artifactsrc.Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeded = append(s.seeded, name)
	return s.err
}

func TestSeedPromptArtifactsNilSource(t *testing.T) {
	// Must not panic or start a goroutine with a nil src.
	seedPromptArtifacts(context.Background(), nil)
}

func TestSeedPromptArtifactsSeedsEveryName(t *testing.T) {
	src := &stubPromptSource{}
	seedPromptArtifacts(context.Background(), src)
	deadline := time.Now().Add(2 * time.Second)
	for {
		src.mu.Lock()
		n := len(src.seeded)
		src.mu.Unlock()
		if n >= len(artifactsrc.Names()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("seeded %d names, want %d", n, len(artifactsrc.Names()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
