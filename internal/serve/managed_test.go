package serve

import (
	"os"
	"strings"
	"testing"
)

// TestStoresComposeEmbedded proves the embed resolved and the file we hand to
// docker is the stores-only stack (db + qdrant), not the repo's dev
// docker-compose.yml (which also has searxng/crawl4ai/app).
func TestStoresComposeEmbedded(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	if err := writeStoresCompose(); err != nil {
		t.Fatalf("writeStoresCompose: %v", err)
	}
	b, err := os.ReadFile(composePath())
	if err != nil {
		t.Fatalf("read %s: %v", composePath(), err)
	}
	s := string(b)
	for _, want := range []string{"quack-stores-pgdata", "qdrant/qdrant", "5432:5432", "6334:6334", "  db:", "  qdrant:"} {
		if !strings.Contains(s, want) {
			t.Errorf("stores compose missing %q", want)
		}
	}
	// Tool backends and the app are NOT managed here (config-driven / separate).
	// Check service keys, not bare substrings - the header comment mentions the
	// backends by name, which is fine.
	for _, notWant := range []string{"  searxng:", "  crawl4ai:", "  app:"} {
		if strings.Contains(s, notWant) {
			t.Errorf("stores compose should not define a %q service (tool backends are config-driven, not managed)", notWant)
		}
	}
}
