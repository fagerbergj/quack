package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// testCloneConfig provisions a Config whose Setup clone dir is a real
// directory tree (files under root/<paths>), so resolveCiteCloneRoots -
// exactly what commitDelivery and citationScore already use - resolves a
// clone root repoPathsResolveCriterion can walk. origin, when non-empty,
// becomes the clone's .git/config remote so cloneRepoIdentities can back a
// GitHub blob URL to it.
func testCloneConfig(t *testing.T, files []string, origin string) Config {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cloneDir, err := j.EnsureDir("u1", "chat1", workspace.SetupCloneDir("impl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		full := filepath.Join(cloneDir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if origin != "" {
		gitDir := filepath.Join(cloneDir, ".git")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := "[remote \"origin\"]\n\turl = " + origin + "\n"
		if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		Setup:           &SetupBranch{Repo: origin, WorkBranch: "quack/work"},
		NodeID:          "impl",
		Workspace:       j,
		WorkspaceUserID: "u1",
		ChatID:          "chat1",
	}
}

// nightsOut65Tree is a slice of the real repo shape from #684's evidence: an
// Android package tree correctly spelled "jasonfagerberg".
var nightsOut65Tree = []string{
	"app/src/main/java/com/wit/jasonfagerberg/MainActivity.kt",
	"app/src/main/java/com/wit/jasonfagerberg/ui/theme/Color.kt",
}

func TestRepoPathsResolveCriterion_BadPathFailsAndIsNamed(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "Update `app/src/main/java/com/wit/jasonfargerberg/ui/theme/Color.kt` to add the new palette entry."
	c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg)
	if !ok {
		t.Fatal("want ok=true - the cited path is misspelled and does not exist in the clone")
	}
	if c.Score != 0 {
		t.Fatalf("Score = %v, want 0", c.Score)
	}
	if !strings.Contains(c.Reason, "jasonfargerberg") {
		t.Errorf("Reason = %q, want it to name the specific unresolved path", c.Reason)
	}
}

// The issue's own worked example: a bare directory citation of the misspelled
// package must fail even though its parent (com/wit) is real - a directory
// citation gets no "not yet created" leniency (see pathIndex.resolves).
func TestRepoPathsResolveCriterion_BareTypoedDirectoryFails(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "Every file moves under `com/wit/jasonfargerberg/`."
	c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg)
	if !ok {
		t.Fatal("want ok=true - com/wit/jasonfargerberg/ does not exist at any depth")
	}
	if !strings.Contains(c.Reason, "com/wit/jasonfargerberg") {
		t.Errorf("Reason = %q, want it to name the bad directory", c.Reason)
	}
}

func TestRepoPathsResolveCriterion_NewFileUnderExistingDirectoryPasses(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "Add a new screen at `app/src/main/java/com/wit/jasonfagerberg/ui/theme/NewScreen.kt`."
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg); ok {
		t.Fatalf("want ok=false (no failure) - the file is new but its directory is real; got %+v", c)
	}
}

func TestRepoPathsResolveCriterion_UnrelatedProjectProseNotTripped(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "This is similar to how Next.js apps put pages under app/router/page.tsx, and/or a Rails app under app/models."
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg); ok {
		t.Fatalf("want ok=false - unquoted prose about another project must never be extracted; got %+v", c)
	}
}

func TestRepoPathsResolveCriterion_GithubBlobURLThisRepoBadPathFails(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "https://github.com/fagerbergj/nightsout.git")
	answer := "See https://github.com/fagerbergj/nightsout/blob/main/app/src/main/java/com/wit/jasonfargerberg/MainActivity.kt for the full listing."
	c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg)
	if !ok {
		t.Fatal("want ok=true - the blob URL is for THIS repo and its path is misspelled")
	}
	if !strings.Contains(c.Reason, "jasonfargerberg") {
		t.Errorf("Reason = %q, want it to name the bad path", c.Reason)
	}
}

func TestRepoPathsResolveCriterion_GithubBlobURLOtherRepoNotTripped(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "https://github.com/fagerbergj/nightsout.git")
	answer := "For reference, see https://github.com/someone-else/other-project/blob/main/totally/different/layout.kt"
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg); ok {
		t.Fatalf("want ok=false - the blob URL names a DIFFERENT repo, must never be checked against this clone; got %+v", c)
	}
}

func TestRepoPathsResolveCriterion_NoCloneSkipsCleanly(t *testing.T) {
	answer := "Update `app/src/main/java/com/wit/jasonfargerberg/MainActivity.kt`."
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, Config{}); ok {
		t.Fatalf("want ok=false - a node with no clone has nothing to resolve against; got %+v", c)
	}
}

func TestRepoPathsResolveCriterion_LineRangeSuffixStripped(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "See `app/src/main/java/com/wit/jasonfagerberg/MainActivity.kt:42` for the crash site."
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg); ok {
		t.Fatalf("want ok=false - the file exists, only a grep-style :42 line suffix is appended; got %+v", c)
	}
}

func TestRepoPathsResolveCriterion_CleanAnswerPasses(t *testing.T) {
	cfg := testCloneConfig(t, nightsOut65Tree, "")
	answer := "Updated `app/src/main/java/com/wit/jasonfagerberg/MainActivity.kt` to fix the crash on rotation."
	if c, ok := repoPathsResolveCriterion(answer, workerActivity{}, cfg); ok {
		t.Fatalf("want ok=false - the cited path is correctly spelled and exists; got %+v", c)
	}
}
