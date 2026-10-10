package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/workspace"
)

func init() { workspace.GitProtocol = "file" } // plugin fixtures are local bare repos

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

// newPluginsTestHandler builds a Handler on a real FSRegistry under t.TempDir() with a rebuild-count hook.
// atomic.Int64 because mapConcurrently can call the hook from several goroutines.
func newPluginsTestHandler(t *testing.T) (*Handler, *atomic.Int64) {
	t.Helper()
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	var rebuilds atomic.Int64
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func(context.Context) (schema.PluginReloadReport, error) {
		rebuilds.Add(1)
		return schema.PluginReloadReport{Generation: rebuilds.Load()}, nil
	}, nil))
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
	if rebuilds.Load() == 0 {
		t.Fatal("expected the skill roster rebuild hook to fire on create")
	}
	if p.Reload == nil || p.Reload.Generation != rebuilds.Load() {
		t.Fatalf("created row reload = %+v, want the reload that followed the add", p.Reload)
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

// mcpJSONBody is a minimal, schema-valid mcp.json declaring one stdio server.
const mcpJSONBody = `{"$schema":"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json","mcpServers":{"foo":{"type":"stdio","command":"echo"}}}`

// TestPluginNoteFlagsDeclaredMCPServersAcrossUpdate: declares_mcp_servers
// tracks the row's CURRENT clone on every Create/Update call.
func TestPluginNoteFlagsDeclaredMCPServersAcrossUpdate(t *testing.T) {
	bare, work := newFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(work, "plugin.json"), []byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"widgets"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pluginregtest.RunGit(t, work, "add", ".")
	pluginregtest.RunGit(t, work, "commit", "--quiet", "-m", "add plugin.json")
	pluginregtest.RunGit(t, work, "push", "--quiet", "origin", "main")
	withFixedRemote(t, bare)

	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	// A live-resolving stand-in, not the real serve-side swap: proves the
	// wire plumbing only, since it re-resolves every call instead of caching.
	mcpDeclared := func() map[string]bool {
		ps, _ := plugin.Resolve([]string{filepath.Join(root, "widgets", "repo")})
		m := map[string]bool{}
		if len(ps) == 1 && len(ps[0].MCPServers) > 0 {
			m["widgets"] = true
		}
		return m
	}
	h.SetPlugins(NewPlugins(reg, root, nil, func(context.Context) (schema.PluginReloadReport, error) { return schema.PluginReloadReport{}, nil }, mcpDeclared))

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	p := decodePlugin(t, w)
	if p.DeclaresMcpServers != nil {
		t.Fatalf("DeclaresMcpServers = %v before mcp.json exists, want unset", p.DeclaresMcpServers)
	}

	if err := os.WriteFile(filepath.Join(work, "mcp.json"), []byte(mcpJSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	pluginregtest.RunGit(t, work, "add", ".")
	pluginregtest.RunGit(t, work, "commit", "--quiet", "-m", "add mcp.json")
	pluginregtest.RunGit(t, work, "push", "--quiet", "origin", "main")

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w = httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusOK {
		t.Fatalf("UpdatePlugin status = %d, body %s", w.Code, w.Body.String())
	}
	p = decodePlugin(t, w)
	if p.DeclaresMcpServers == nil || !*p.DeclaresMcpServers {
		t.Fatalf("DeclaresMcpServers = %v after mcp.json is added and fetched, want true", p.DeclaresMcpServers)
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
	rebuilds.Store(0)

	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if rebuilds.Load() == 0 {
		t.Fatal("expected the skill roster rebuild hook to fire on delete")
	}
	var deleted schema.PluginDeleted
	if err := json.NewDecoder(w.Body).Decode(&deleted); err != nil || deleted.Reload.Generation != rebuilds.Load() {
		t.Fatalf("delete body = %+v (%v), want the reload that followed", deleted, err)
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

// TestUpdatePluginInstallsNewShaAndServesNewText: after a push, POST .../update installs the new sha
// and the clone on disk (what a live skill.Source reads) carries the new content.
func TestUpdatePluginInstallsNewShaAndServesNewText(t *testing.T) {
	bare, work := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, rebuilds := newPluginsTestHandler(t)
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)

	newSHA := commitAndPush(t, work, "v2")
	rebuilds.Store(0)

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
	if rebuilds.Load() == 0 {
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

// TestCreatePluginRejectsNonGitHubEntries: REST manages github: entries only; a bare string, absolute path,
// https URL or whitespace must never become a local-root row.
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

// TestPutPreservesShaOnFailedRefetch: re-POSTing an installed entry keeps installed_sha/fetched_at
// when the following Fetch fails, since the last good clone still serves that sha.
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

// TestCreatePluginNameCollisionIs409: only pluginreg.ErrNameCollision maps to 409;
// anything else is a 500.
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

// TestCreatePluginRejectsReservedNames: "update"/"updates" would collide with the fixed REST path segment.
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

// TestUpdateAllPluginsFetchesOnlyBehindRows: a row CheckUpdate reports current isn't re-fetched
// (fetched_at stays put); only the behind row's Fetch runs.
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

	// CheckUpdate/Fetch resolve each row through pluginreg.RemoteURL by owner/repo,
	// so route each name to its own fixture bare repo.
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

// TestRebuildRefusalIs422AndStoresError: a rebuild refusal naming the just-created row 422s with its message.
// Persisting the refusal is admitPlugins' job (serve's TestRebuildSkillsDropsOnlyTheRefusedRow).
func TestRebuildRefusalIs422AndStoresError(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	name := "widgets"
	refused := schema.PluginReloadFailure{Plugin: &name, Stage: schema.Admission, Error: `plugin "widgets" declares module "x", which is not linked`}
	h.SetPlugins(NewPlugins(reg, root, nil, func(context.Context) (schema.PluginReloadReport, error) {
		return schema.PluginReloadReport{Failures: []schema.PluginReloadFailure{refused}}, nil
	}, nil))

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not linked") {
		t.Fatalf("422 body should carry the refusal, got %s", w.Body.String())
	}
}

// POST /plugins/reload: 200 with the report, or 422 with the same shape when nothing swapped.
func TestReloadPlugins(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	plugin := "p"
	failed := schema.PluginReloadReport{Generation: 4, Failures: []schema.PluginReloadFailure{{Plugin: &plugin, Stage: schema.Registry, Error: "registry down"}}}
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"swapped", nil, http.StatusOK},
		{"aborted", errors.New("registry down"), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			h.SetPlugins(NewPlugins(reg, root, nil, func(context.Context) (schema.PluginReloadReport, error) { return failed, tc.err }, nil))
			w := doJSON(t, h.ReloadPlugins, http.MethodPost, "")
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			var got schema.PluginReloadReport
			if err := json.NewDecoder(w.Body).Decode(&got); err != nil || got.Generation != 4 || len(got.Failures) != 1 {
				t.Fatalf("body = %+v (%v), want the reload report", got, err)
			}
		})
	}
	w := doJSON(t, (&Handler{}).ReloadPlugins, http.MethodPost, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unwired status = %d, want 500", w.Code)
	}
}

func TestCreatePluginRefusesReloadName(t *testing.T) {
	h, _ := newPluginsTestHandler(t)
	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/reload"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for the reserved name", w.Code)
	}
}

// A create whose reload aborts answers 422 with the report, and the row stays registered.
func TestCreatePluginReloadAbortKeepsRowAndReports(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func(context.Context) (schema.PluginReloadReport, error) {
		rep := NewReloadReport(3)
		rep.Failures = append(rep.Failures, schema.PluginReloadFailure{Stage: schema.Registry, Error: "registry down"})
		return rep, errors.New("registry down")
	}, nil))
	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var body schema.PluginReloadError
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || body.Error != "registry down" || body.Reload.Generation != 3 || len(body.Reload.Failures) != 1 {
		t.Fatalf("body = %+v (%v), want the message and the reload report", body, err)
	}
	rows, err := reg.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Name != "widgets" {
		t.Fatalf("rows = %+v (%v), want widgets kept", rows, err)
	}
}

func TestReloadUnwiredReportsEmptyLists(t *testing.T) {
	root := t.TempDir()
	h := &Handler{}
	h.SetPlugins(NewPlugins(pluginreg.NewFSRegistry(root), root, nil, nil, nil))
	w := doJSON(t, h.ReloadPlugins, http.MethodPost, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "null") {
		t.Fatalf("unwired reload = %d %s, want 200 with empty lists", w.Code, w.Body.String())
	}
}

// A local root is config-only: DELETE refuses it and the row survives.
func TestDeletePluginRefusesLocalRoot(t *testing.T) {
	h, _ := newPluginsTestHandler(t)
	root := t.TempDir()
	row := pluginreg.Plugin{Name: filepath.Base(root), Source: pluginreg.SourceLocal, Entry: root}
	if err := h.plugins.reg.Put(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.DeletePlugin(w, httptest.NewRequest(http.MethodDelete, "/", nil), row.Name)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "plugins.seed") {
		t.Fatalf("status = %d %s, want 409 naming plugins.seed", w.Code, w.Body.String())
	}
	if rows, _ := h.plugins.reg.List(context.Background()); len(rows) != 1 {
		t.Fatalf("rows = %+v, want the local row kept", rows)
	}
}

// A POST takes a seeded row over from config, so plugins.seed stops moving it.
func TestCreatePluginClearsSeeded(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h, _ := newPluginsTestHandler(t)
	ctx := context.Background()
	if err := h.plugins.reg.Put(ctx, pluginreg.Plugin{Name: "widgets", Source: pluginreg.SourceGitHub, Entry: "github:acme/widgets@v1", Owner: "acme", Repo: "widgets", Ref: "v1", Seeded: true}); err != nil {
		t.Fatal(err)
	}
	var list schema.PluginList
	if err := json.NewDecoder(doJSON(t, h.ListPlugins, http.MethodGet, "").Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if w, ok := findWirePlugin(list.Plugins, "widgets"); !ok || w.Seeded == nil || !*w.Seeded {
		t.Fatalf("listed widgets = %+v, want seeded on the wire", w)
	}
	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreatePlugin status = %d, body %s", w.Code, w.Body.String())
	}
	if p := decodePlugin(t, w); p.Seeded != nil {
		t.Fatalf("created row = %+v, want seeded absent", p)
	}
	rows, err := h.plugins.reg.List(ctx)
	if err != nil || len(rows) != 1 || rows[0].Seeded || rows[0].Ref != "" {
		t.Fatalf("rows = %+v, err %v; want widgets tracked and no longer seeded", rows, err)
	}
}
