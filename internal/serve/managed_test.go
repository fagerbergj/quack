package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoresComposeEmbedded proves the embed resolved and the file we hand to
// docker is the stores-only stack (db + qdrant), not the repo's dev
// docker-compose.yml (which also has searxng/crawl4ai/app).
func TestStoresComposeEmbedded(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
	// Check service keys, not bare substrings — the header comment mentions the
	// backends by name, which is fine.
	for _, notWant := range []string{"  searxng:", "  crawl4ai:", "  app:"} {
		if strings.Contains(s, notWant) {
			t.Errorf("stores compose should not define a %q service (tool backends are config-driven, not managed)", notWant)
		}
	}
}

// TestStateTopologyRoundTrip covers the state-file format change: PID ADDR
// TOPOLOGY. The topology field is what lets `server stop` know it should tear
// down the managed stores, and it's optional (older state files omit it).
func TestStateTopologyRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := writeState(12345, ":8080", "managed"); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	pid, addr, topo, ok := readState()
	if !ok || pid != 12345 || addr != ":8080" || topo != "managed" {
		t.Errorf("readState = %d %q %q ok=%v, want 12345 :8080 managed", pid, addr, topo, ok)
	}

	// Non-managed run records an empty topology.
	if err := writeState(99, ":9090", ""); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	_, _, topo2, _ := readState()
	if topo2 != "" {
		t.Errorf("readState topology = %q, want empty for non-managed", topo2)
	}

	// Backward compat: a legacy 2-field "PID ADDR" state file parses with an
	// empty topology (so an upgrade doesn't lose the recorded daemon).
	legacy := filepath.Join(stateDir(), "server.pid")
	if err := os.WriteFile(legacy, []byte("42 :7070\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid3, addr3, topo3, ok3 := readState()
	if !ok3 || pid3 != 42 || addr3 != ":7070" || topo3 != "" {
		t.Errorf("legacy state = %d %q %q ok=%v, want 42 :7070 empty", pid3, addr3, topo3, ok3)
	}
}
