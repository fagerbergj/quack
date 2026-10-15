package serve

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/workspace"
)

// The sweep takes an old experiment chat's scope and node dirs, keeping a fresh one (a live item) and any other chat's.
func TestSweepExperimentWorkspaces(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, err := jail.HomeDir(localUserID)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(chatID string, age time.Duration) (scope, node string) {
		dir := workspace.ChatDirName(chatID)
		scope, node = filepath.Join(jail.Root(), localUserID, dir), filepath.Join(home, "tmp", dir+"__n1")
		for _, p := range []string{scope, node} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(scope, old, old); err != nil {
			t.Fatal(err)
		}
		return scope, node
	}
	staleScope, staleNode := mk(store.ExperimentChatPrefix+"stale", 7*time.Hour)
	liveScope, _ := mk(store.ExperimentChatPrefix+"live", time.Minute)
	plainScope, _ := mk("chat-plain", 7*time.Hour)

	if n := SweepExperimentWorkspaces(jail, 6*time.Hour); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for p, want := range map[string]bool{staleScope: false, staleNode: false, liveScope: true, plainScope: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
}
