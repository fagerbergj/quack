package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/schema"
)

// newFixtureRepo/commitAndPush are pluginregtest's shared git fixture -
// see internal/pluginreg/fetch_test.go for the same wrapping.
func newFixtureRepo(t *testing.T) (bare, work string) { return pluginregtest.NewFixtureRepo(t) }

func commitAndPush(t *testing.T, work, content string) string {
	return pluginregtest.CommitAndPush(t, work, content)
}

// withFixedRemote overrides pluginreg.RemoteURL to resolve every owner/repo
// to url (a local bare fixture), restored on cleanup.
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
	if !slices.Contains(names, "widgets") || !slices.Contains(names, "quack") {
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

// TestCreatePluginRejectsNonGitHubEntries is the adversarial-review severe#1
// regression: REST manages github: entries only - a bare string, an
// absolute path, an https URL and whitespace must never become a local-root
// row (they used to 201 and enter the skill roots).
func TestCreatePluginRejectsNonGitHubEntries(t *testing.T) {
	for _, entry := range []string{"just-garbage", "/etc", "https://github.com/a/b", "  "} {
		t.Run(entry, func(t *testing.T) {
			h, _ := newPluginsTestHandler(t)
			w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":`+jsonStr(entry)+`}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("entry %q status = %d, body %s, want 400", entry, w.Code, w.Body.String())
			}
			rows, err := h.plugins.reg.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("entry %q was rejected but still created a row: %+v", entry, rows)
			}
		})
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestPutPreservesShaOnFailedRefetch is the adversarial-review severe#2
// regression: re-POSTing an already-installed entry must not wipe
// installed_sha/fetched_at when the immediately-following Fetch fails - the
// last good clone still serves that sha (replay, ledger provenance, P4).
func TestPutPreservesShaOnFailedRefetch(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, _ := newPluginsTestHandler(t)

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	first := decodePlugin(t, w)
	if first.InstalledSha == nil || *first.InstalledSha == "" {
		t.Fatalf("first create didn't install a sha: %+v", first)
	}
	wantSHA, wantFetchedAt := *first.InstalledSha, *first.FetchedAt

	// Break the remote, then re-POST the SAME entry - Put runs before the
	// now-failing Fetch, and must not have already cleared the row.
	withFixedRemote(t, filepath.Join(t.TempDir(), "does-not-exist.git"))
	w = doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	second := decodePlugin(t, w)
	if second.Error == nil || *second.Error == "" {
		t.Fatalf("expected the broken re-fetch to set error, got %+v", second)
	}
	if second.InstalledSha == nil || *second.InstalledSha != wantSHA {
		t.Fatalf("installed_sha after a failed re-fetch = %v, want preserved %q", second.InstalledSha, wantSHA)
	}
	if second.FetchedAt == nil || !second.FetchedAt.Equal(wantFetchedAt) {
		t.Fatalf("fetched_at after a failed re-fetch = %v, want preserved %v", second.FetchedAt, wantFetchedAt)
	}
}

// TestCreatePluginNameCollisionIs409 is should-fix#6: only a genuine
// identity collision maps to 409 (pluginreg.ErrNameCollision), everything
// else is a 500.
func TestCreatePluginNameCollisionIs409(t *testing.T) {
	bare1, _ := newFixtureRepo(t)
	h, _ := newPluginsTestHandler(t)
	withFixedRemote(t, bare1)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:someoneelse/widgets"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want 409", w.Code, w.Body.String())
	}
}

// TestDeletePluginInvalidNameIs400 is should-fix#7: a path-unsafe {name}
// (traversal or nonsense) must 400, not 500.
func TestDeletePluginInvalidNameIs400(t *testing.T) {
	h, _ := newPluginsTestHandler(t)
	for _, name := range []string{"..", "a/../../victim", "."} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodDelete, "/", nil)
			w := httptest.NewRecorder()
			h.DeletePlugin(w, r, name)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("name %q status = %d, body %s, want 400", name, w.Code, w.Body.String())
			}
		})
	}
}

// TestCreatePluginRejectsReservedNames is should-fix#10: a plugin named
// "update"/"updates" would collide with the fixed REST path segment.
func TestCreatePluginRejectsReservedNames(t *testing.T) {
	for _, entry := range []string{"github:acme/update", "github:acme/updates"} {
		t.Run(entry, func(t *testing.T) {
			h, _ := newPluginsTestHandler(t)
			w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"`+entry+`"}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("entry %q status = %d, body %s, want 400", entry, w.Code, w.Body.String())
			}
		})
	}
}

// TestUpdateAllPluginsFetchesOnlyBehindRows is should-fix#8: a row CheckUpdate
// reports current must not be re-fetched (its fetched_at stays put); only
// the behind row's Fetch (and fetched_at) actually runs.
func TestUpdateAllPluginsFetchesOnlyBehindRows(t *testing.T) {
	staleBare, staleWork := newFixtureRepo(t)
	currentBare, _ := newFixtureRepo(t)

	h, _ := newPluginsTestHandler(t)
	withFixedRemote(t, staleBare)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/stale"}`)
	withFixedRemote(t, currentBare)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/current"}`)

	rows, err := h.plugins.reg.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	staleBefore, _ := findPluginRow(rows, "stale")
	currentBefore, _ := findPluginRow(rows, "current")

	commitAndPush(t, staleWork, "v2") // only "stale" is now behind

	// CheckUpdate/Fetch resolve per-row through pluginreg.RemoteURL by
	// owner/repo, not a single global override - route each name to its
	// own fixture bare repo.
	prevRemoteURL := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string {
		if repo == "stale" {
			return staleBare
		}
		return currentBare
	}
	t.Cleanup(func() { pluginreg.RemoteURL = prevRemoteURL })

	w := doJSON(t, h.UpdateAllPlugins, http.MethodPost, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var list schema.PluginList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	stale, ok := findWirePlugin(list.Plugins, "stale")
	if !ok || stale.InstalledSha == nil || *stale.InstalledSha == staleBefore.SHA {
		t.Fatalf("stale row = %+v, want a new sha (it was behind, before sha %q)", stale, staleBefore.SHA)
	}
	current, ok := findWirePlugin(list.Plugins, "current")
	if !ok || current.FetchedAt == nil || !current.FetchedAt.Equal(*currentBefore.FetchedAt) {
		t.Fatalf("current row's fetched_at changed = %+v, want unchanged (it was not behind, so never Fetched)", current)
	}
}

func findWirePlugin(rows []schema.Plugin, name string) (schema.Plugin, bool) {
	for _, p := range rows {
		if p.Name == name {
			return p, true
		}
	}
	return schema.Plugin{}, false
}

// TestRebuildRefusalIs422AndStoresError is should-fix#9: a rebuild refusal
// (e.g. checkPluginModules) must 422, store the refusal on the row, and
// never silently admit a bad plugin the way boot would refuse it.
func TestRebuildRefusalIs422AndStoresError(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() error {
		return errors.New("plugin \"widgets\" declares module \"x\", which is not linked")
	}))

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not linked") {
		t.Fatalf("422 body should carry the refusal, got %s", w.Body.String())
	}
	rows, err := reg.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := findPluginRow(rows, "widgets")
	if !ok || row.Error == "" {
		t.Fatalf("row after a refused rebuild = %+v, want error stored", row)
	}
}
