package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
)

// The fixture/remote-override helpers below mirror internal/pluginreg's own
// _test.go helpers (unexported there, so not importable across packages) -
// same pattern: a local bare repo stands in for github.com via
// pluginreg.RemoteURL, no network involved.

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newFixtureRepo makes a bare "remote" repo plus a pushing work tree, and
// returns (bare repo path, work tree path).
func newFixtureRepo(t *testing.T) (bare, work string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)

	work = t.TempDir()
	runGit(t, work, "init", "--quiet", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "v1")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "--quiet", "origin", "main")
	return bare, work
}

func commitAndPush(t *testing.T, work, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", content)
	runGit(t, work, "push", "--quiet", "origin", "main")
	return strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))
}

func withFixedRemote(t *testing.T, url string) {
	t.Helper()
	prev := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string { return url }
	t.Cleanup(func() { pluginreg.RemoteURL = prev })
}

// newPluginsTestHandler builds a Handler wired to a real FSRegistry rooted
// under t.TempDir(), with a rebuild-count hook so tests can assert the
// native skill roster is rebuilt on every row/sha change.
func newPluginsTestHandler(t *testing.T) (*Handler, *int) {
	t.Helper()
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	rebuilds := 0
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() error {
		rebuilds++
		return nil
	}))
	return h, &rebuilds
}

func doJSON(t *testing.T, h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/", bytes.NewBufferString(body))
	} else {
		r = httptest.NewRequest(method, "/", nil)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func decodePlugin(t *testing.T, w *httptest.ResponseRecorder) schema.Plugin {
	t.Helper()
	var p schema.Plugin
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatalf("decode Plugin: %v\nbody: %s", err, w.Body.String())
	}
	return p
}

func TestCreatePluginRoundTrip(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, rebuilds := newPluginsTestHandler(t)

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreatePlugin status = %d, body %s", w.Code, w.Body.String())
	}
	p := decodePlugin(t, w)
	if p.Name != "widgets" || p.Error != nil {
		t.Fatalf("created row = %+v", p)
	}
	if p.InstalledSha == nil || *p.InstalledSha == "" {
		t.Fatalf("expected installed_sha to be set, got %+v", p)
	}
	if *rebuilds == 0 {
		t.Fatal("expected the skill roster rebuild hook to fire on create")
	}

	w = doJSON(t, h.ListPlugins, http.MethodGet, "")
	var list schema.PluginList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range list.Plugins {
		names = append(names, row.Name)
	}
	if !slicesContain(names, "widgets") || !slicesContain(names, "quack") {
		t.Fatalf("ListPlugins = %v, want widgets and the embedded quack row", names)
	}
}

func TestCreatePluginBadEntry(t *testing.T) {
	h, _ := newPluginsTestHandler(t)
	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:no-slash"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "github:owner/repo") {
		t.Fatalf("400 body should name the syntax, got %s", w.Body.String())
	}
}

func TestDeletePlugin(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, rebuilds := newPluginsTestHandler(t)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	*rebuilds = 0

	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if *rebuilds == 0 {
		t.Fatal("expected the skill roster rebuild hook to fire on delete")
	}

	w = httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", w.Code)
	}

	w = httptest.NewRecorder()
	h.DeletePlugin(w, r, "quack")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete quack status = %d, want 400", w.Code)
	}
}

func TestListPluginUpdatesShowsBehindAfterPush(t *testing.T) {
	bare, work := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, _ := newPluginsTestHandler(t)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)

	newSHA := commitAndPush(t, work, "v2")

	w := doJSON(t, h.ListPluginUpdates, http.MethodGet, "")
	var list schema.PluginUpdateList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Updates) != 1 {
		t.Fatalf("updates = %+v, want exactly one row", list.Updates)
	}
	u := list.Updates[0]
	if !u.Behind || u.RemoteSha == nil || *u.RemoteSha != newSHA {
		t.Fatalf("update row = %+v, want behind=true remote_sha=%s", u, newSHA)
	}
}

// TestUpdatePluginInstallsNewShaAndServesNewText proves the P2 verification
// requirement literally: after a push, POST .../update installs the new sha
// AND the clone on disk (what any live skill.Source reads) carries the new
// content - the "next run loads the new text" the issue asks to prove.
func TestUpdatePluginInstallsNewShaAndServesNewText(t *testing.T) {
	bare, work := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, rebuilds := newPluginsTestHandler(t)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)

	newSHA := commitAndPush(t, work, "v2")
	*rebuilds = 0

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	p := decodePlugin(t, w)
	if p.InstalledSha == nil || *p.InstalledSha != newSHA {
		t.Fatalf("installed_sha = %v, want %s", p.InstalledSha, newSHA)
	}
	if *rebuilds == 0 {
		t.Fatal("expected the skill roster rebuild hook to fire on update")
	}

	row := pluginreg.Plugin{Name: "widgets", Source: pluginreg.SourceGitHub}
	got, err := os.ReadFile(filepath.Join(row.Root(h.plugins.root), "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("clone content = %q, want %q (the pushed text)", got, "v2")
	}
}

func TestUpdatePluginNotFound(t *testing.T) {
	h, _ := newPluginsTestHandler(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func slicesContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
