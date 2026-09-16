package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
)

// failingRegistry is a pluginRegistry whose every method returns a
// caller-set error (or delegates CheckUpdate to checkFn, for the budget
// test) - the "failing registry stub" the adversarial review asked for,
// exercising error branches no real FSRegistry fixture can force on demand.
type failingRegistry struct {
	rows      []pluginreg.Plugin
	listErr   error
	putErr    error
	deleteErr error
	fetchErr  error
	checkErr  error
	checkFn   func(ctx context.Context, p pluginreg.Plugin) (bool, string, error)
}

func (f *failingRegistry) List(context.Context) ([]pluginreg.Plugin, error) { return f.rows, f.listErr }
func (f *failingRegistry) Put(context.Context, pluginreg.Plugin) error      { return f.putErr }
func (f *failingRegistry) Delete(context.Context, string) error             { return f.deleteErr }
func (f *failingRegistry) Fetch(_ context.Context, p pluginreg.Plugin) (pluginreg.Plugin, error) {
	return p, f.fetchErr
}
func (f *failingRegistry) CheckUpdate(ctx context.Context, p pluginreg.Plugin) (bool, string, error) {
	if f.checkFn != nil {
		return f.checkFn(ctx, p)
	}
	return false, "", f.checkErr
}

func handlerWith(reg pluginRegistry) *Handler {
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, "/root", nil, nil))
	return h
}

// TestPluginHandlersRequirePluginsConfigured: every handler 500s with a
// clear message instead of a nil-pointer panic when boot never wired
// SetPlugins - requirePlugins' guard, exercised from each call site.
func TestPluginHandlersRequirePluginsConfigured(t *testing.T) {
	h := &Handler{}
	getCases := map[string]http.HandlerFunc{
		"ListPlugins": h.ListPlugins, "CreatePlugin": h.CreatePlugin,
		"ListPluginUpdates": h.ListPluginUpdates, "UpdateAllPlugins": h.UpdateAllPlugins,
	}
	for name, fn := range getCases {
		t.Run(name, func(t *testing.T) {
			w := doJSON(t, fn, http.MethodGet, "")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
		})
	}
	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "x")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("DeletePlugin status = %d, want 500", w.Code)
	}
	w = httptest.NewRecorder()
	h.UpdatePlugin(w, r, "x")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("UpdatePlugin status = %d, want 500", w.Code)
	}
}

func TestListPlugins500OnRegistryListError(t *testing.T) {
	h := handlerWith(&failingRegistry{listErr: errors.New("disk error")})
	w := doJSON(t, h.ListPlugins, http.MethodGet, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

// TestCreatePlugin500OnNonCollisionPutError: a Put failure that is NOT
// pluginreg.ErrNameCollision must not be misreported as a 409.
func TestCreatePlugin500OnNonCollisionPutError(t *testing.T) {
	h := handlerWith(&failingRegistry{putErr: errors.New("disk full")})
	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

func TestDeletePlugin500OnGenericRegistryError(t *testing.T) {
	h := handlerWith(&failingRegistry{deleteErr: errors.New("disk error")})
	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

// TestDeletePluginRebuildFailureIsWarnOnly: the delete already committed, so
// a rebuild failure only logs - the response still reports success.
func TestDeletePluginRebuildFailureIsWarnOnly(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	if err := reg.Put(context.Background(), pluginreg.Plugin{Name: "widgets", Source: pluginreg.SourceLocal, Entry: "widgets"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() (map[string]error, error) { return nil, errors.New("roster refused") }))
	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s, want 204 (delete succeeds regardless of rebuild)", w.Code, w.Body.String())
	}
}

func TestListPluginUpdates500OnRegistryListError(t *testing.T) {
	h := handlerWith(&failingRegistry{listErr: errors.New("disk error")})
	w := doJSON(t, h.ListPluginUpdates, http.MethodGet, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

func TestListPluginUpdatesReportsPerRowCheckError(t *testing.T) {
	h := handlerWith(&failingRegistry{
		rows:     []pluginreg.Plugin{{Name: "widgets", Source: pluginreg.SourceGitHub}},
		checkErr: errors.New("remote unreachable"),
	})
	w := doJSON(t, h.ListPluginUpdates, http.MethodGet, "")
	var list schema.PluginUpdateList
	decodeJSON(t, w, &list)
	if len(list.Updates) != 1 || list.Updates[0].Error == nil {
		t.Fatalf("updates = %+v, want one row with error set", list.Updates)
	}
}

func TestUpdatePlugin500OnRegistryListError(t *testing.T) {
	h := handlerWith(&failingRegistry{listErr: errors.New("disk error")})
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

// TestUpdatePluginRebuildRefusalIs422: UpdatePlugin's own rebuild-refusal
// branch (CreatePlugin's is covered by TestRebuildRefusalIs422AndStoresError).
func TestUpdatePluginRebuildRefusalIs422(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() (map[string]error, error) { return nil, errors.New("roster refused") }))
	doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`) // already 422s, row still stored

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
}

func TestUpdateAllPlugins500OnRegistryListError(t *testing.T) {
	h := handlerWith(&failingRegistry{listErr: errors.New("disk error")})
	w := doJSON(t, h.UpdateAllPlugins, http.MethodPost, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s, want 500", w.Code, w.Body.String())
	}
}

func TestUpdateAllPluginsReportsPerRowCheckError(t *testing.T) {
	h := handlerWith(&failingRegistry{
		rows:     []pluginreg.Plugin{{Name: "widgets", Source: pluginreg.SourceGitHub}},
		checkErr: errors.New("remote unreachable"),
	})
	w := doJSON(t, h.UpdateAllPlugins, http.MethodPost, "")
	var list schema.PluginList
	decodeJSON(t, w, &list)
	if len(list.Plugins) != 1 || list.Plugins[0].Error == nil {
		t.Fatalf("plugins = %+v, want one row with error set (checked, not fetched)", list.Plugins)
	}
}

func TestUpdateAllPluginsRebuildRefusalIs422(t *testing.T) {
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() (map[string]error, error) { return nil, errors.New("roster refused") }))
	w := doJSON(t, h.UpdateAllPlugins, http.MethodPost, "")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s, want 422", w.Code, w.Body.String())
	}
}

// TestRebuildNilReceiverIsNoop: (*Plugins)(nil).rebuild() is the same
// defensive nil-guard requirePlugins already enforces at the HTTP layer -
// a second line of defense, cheap to prove directly.
func TestRebuildNilReceiverIsNoop(t *testing.T) {
	var p *Plugins
	if refusals, err := p.rebuild(); err != nil || refusals != nil {
		t.Fatalf("rebuild() on a nil *Plugins = (%v, %v), want (nil, nil)", refusals, err)
	}
}

func TestPluginWireIncludesRefAndPath(t *testing.T) {
	w := pluginWire("/root", pluginreg.Plugin{
		Name: "widgets", Source: pluginreg.SourceGitHub, Ref: "main", Path: "skills",
	})
	if w.Ref == nil || *w.Ref != "main" {
		t.Errorf("Ref = %v, want main", w.Ref)
	}
	if w.Path == nil || *w.Path != "skills" {
		t.Errorf("Path = %v, want skills", w.Path)
	}
}

// TestPluginUpdateBudgetExpires proves ListPluginUpdates/UpdateAllPlugins'
// context.WithTimeout actually bounds a slow row: shrink the budget, make
// CheckUpdate block past it, and confirm the per-row result carries the
// deadline error instead of hanging for the real 30s.
func TestPluginUpdateBudgetExpires(t *testing.T) {
	prev := pluginUpdateBudget
	pluginUpdateBudget = 10 * time.Millisecond
	t.Cleanup(func() { pluginUpdateBudget = prev })

	h := handlerWith(&failingRegistry{
		rows: []pluginreg.Plugin{{Name: "widgets", Source: pluginreg.SourceGitHub}},
		checkFn: func(ctx context.Context, p pluginreg.Plugin) (bool, string, error) {
			<-ctx.Done()
			return false, "", ctx.Err()
		},
	})
	start := time.Now()
	w := doJSON(t, h.ListPluginUpdates, http.MethodGet, "")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v, want the shrunk budget to bound it well under a second", elapsed)
	}
	var list schema.PluginUpdateList
	decodeJSON(t, w, &list)
	if len(list.Updates) != 1 || list.Updates[0].Error == nil {
		t.Fatalf("updates = %+v, want the deadline error reported per-row", list.Updates)
	}
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
}

// TestListPluginsShadowedEmbeddedRowNotDuplicated is review#4: a real row
// already named "quack" (it shadows the embedded baseline, epic #1427 S2)
// must appear once, never alongside a synthetic embedded row of the same name.
func TestListPluginsShadowedEmbeddedRowNotDuplicated(t *testing.T) {
	h := handlerWith(&failingRegistry{
		rows: []pluginreg.Plugin{{Name: "quack", Source: pluginreg.SourceGitHub, Owner: "acme", Repo: "quack"}},
	})
	w := doJSON(t, h.ListPlugins, http.MethodGet, "")
	var list schema.PluginList
	decodeJSON(t, w, &list)
	count := 0
	for _, p := range list.Plugins {
		if p.Name == "quack" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("plugins named quack = %d, want exactly 1 (the shadowing row, no synthetic embedded duplicate): %+v", count, list.Plugins)
	}
	if list.Plugins[0].Source != "github" {
		t.Errorf("the one quack row's source = %q, want github (the real row wins, not embedded)", list.Plugins[0].Source)
	}
}
