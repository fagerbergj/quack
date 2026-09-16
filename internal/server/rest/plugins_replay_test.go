package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/replay"
)

// countingRegistry wraps a real pluginRegistry and counts mutating calls -
// proof that a pinned 409 (#1427 P4 F1) happens BEFORE any Put/Fetch/Delete
// reaches the registry, not merely before the HTTP response is written.
type countingRegistry struct {
	pluginRegistry
	puts, fetches, deletes int
}

func (c *countingRegistry) Put(ctx context.Context, p pluginreg.Plugin) error {
	c.puts++
	return c.pluginRegistry.Put(ctx, p)
}

func (c *countingRegistry) Fetch(ctx context.Context, p pluginreg.Plugin) (pluginreg.Plugin, error) {
	c.fetches++
	return c.pluginRegistry.Fetch(ctx, p)
}

func (c *countingRegistry) Delete(ctx context.Context, name string) error {
	c.deletes++
	return c.pluginRegistry.Delete(ctx, name)
}

// newPinnedPluginsTestHandler is newPluginsTestHandler's replay-pinned twin:
// SetPinnedBundle marks the roster pinned exactly as initSkills does once a
// replay bundle swapped in the skill source; rebuildSkills fails the test if
// ever called - requireMutable must refuse before reaching it.
func newPinnedPluginsTestHandler(t *testing.T, root string) (*Handler, *countingRegistry) {
	t.Helper()
	reg := &countingRegistry{pluginRegistry: pluginreg.NewFSRegistry(root)}
	h := &Handler{}
	p := NewPlugins(reg, root, nil, func() (map[string]error, error) {
		t.Fatal("rebuildSkills must not be called while the roster is pinned")
		return nil, nil
	})
	p.SetPinnedBundle("/tmp/x.jsonl")
	h.SetPlugins(p)
	return h, reg
}

// seedFetchedWidgets registers and fetches "widgets" against an UNPINNED
// handler over root, so a later pinned test starts from a real row+clone on
// disk to prove untouched, not an empty registry.
func seedFetchedWidgets(t *testing.T, root string) (sha string) {
	t.Helper()
	reg := pluginreg.NewFSRegistry(root)
	got, err := reg.Fetch(context.Background(), pluginreg.FromEntry(mustParseEntry(t, "github:acme/widgets")))
	if err != nil {
		t.Fatal(err)
	}
	return got.SHA
}

// TestCreatePlugin_ReplayPinnedRefuses409 is F1: a pinned roster refuses
// BEFORE Put/Fetch - no row is created, not merely a failed-looking response.
func TestCreatePlugin_ReplayPinnedRefuses409(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	h, reg := newPinnedPluginsTestHandler(t, root)

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if reg.puts != 0 || reg.fetches != 0 {
		t.Fatalf("puts=%d fetches=%d, want 0/0 (nothing should reach the registry)", reg.puts, reg.fetches)
	}
	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findPluginRow(rows, "widgets"); ok {
		t.Fatal("widgets row exists after a refused create")
	}
}

// TestUpdatePlugin_ReplayPinnedRefuses409 is F1: an existing row's installed
// sha must be untouched by a refused update - no Fetch reaches the registry.
func TestUpdatePlugin_ReplayPinnedRefuses409(t *testing.T) {
	bare, work := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	beforeSHA := seedFetchedWidgets(t, root)
	commitAndPush(t, work, "moved on") // registry now behind - a real Fetch would install a new sha

	h, reg := newPinnedPluginsTestHandler(t, root)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if reg.fetches != 0 {
		t.Fatalf("fetches = %d, want 0", reg.fetches)
	}
	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := findPluginRow(rows, "widgets")
	if !ok {
		t.Fatal("widgets row missing")
	}
	if row.SHA != beforeSHA {
		t.Fatalf("installed sha = %q, want unchanged %q", row.SHA, beforeSHA)
	}
}

// TestUpdateAllPlugins_ReplayPinnedRefuses409 mirrors UpdatePlugin's case
// for the "(all behind)" endpoint.
func TestUpdateAllPlugins_ReplayPinnedRefuses409(t *testing.T) {
	bare, work := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	beforeSHA := seedFetchedWidgets(t, root)
	commitAndPush(t, work, "moved on")

	h, reg := newPinnedPluginsTestHandler(t, root)
	w := doJSON(t, h.UpdateAllPlugins, http.MethodPost, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if reg.fetches != 0 {
		t.Fatalf("fetches = %d, want 0", reg.fetches)
	}
	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, _ := findPluginRow(rows, "widgets")
	if row.SHA != beforeSHA {
		t.Fatalf("installed sha = %q, want unchanged %q", row.SHA, beforeSHA)
	}
}

// TestDeletePlugin_ReplayPinnedRefuses409 is F1's severe case: a pinned
// roster must not delete the row OR its clone off disk.
func TestDeletePlugin_ReplayPinnedRefuses409(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	seedFetchedWidgets(t, root)
	cloneDir := pluginreg.CloneDir(root, "widgets")
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("test setup: clone missing at %s: %v", cloneDir, err)
	}

	h, reg := newPinnedPluginsTestHandler(t, root)
	r := httptest.NewRequest(http.MethodDelete, "/", nil)
	w := httptest.NewRecorder()
	h.DeletePlugin(w, r, "widgets")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if reg.deletes != 0 {
		t.Fatalf("deletes = %d, want 0", reg.deletes)
	}
	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findPluginRow(rows, "widgets"); !ok {
		t.Fatal("widgets row deleted despite the pin")
	}
	if _, err := os.Stat(cloneDir); err != nil {
		t.Fatalf("clone deleted despite the pin: %v", err)
	}
}

func TestRebuildStatus(t *testing.T) {
	if got := rebuildStatus(fmt.Errorf("wrap: %w", replay.ErrPinned)); got != http.StatusConflict {
		t.Fatalf("rebuildStatus(wrapped ErrPinned) = %d, want 409", got)
	}
	if got := rebuildStatus(errors.New("some other refusal")); got != http.StatusUnprocessableEntity {
		t.Fatalf("rebuildStatus(other) = %d, want 422", got)
	}
}

func mustParseEntry(t *testing.T, s string) pluginreg.Entry {
	t.Helper()
	e, err := pluginreg.ParseEntry(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
