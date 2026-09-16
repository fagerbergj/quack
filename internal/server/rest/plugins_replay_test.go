package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/replay"
)

// newPinnedPluginsTestHandler is newPluginsTestHandler's replay-pinned twin
// (#1427 P4 review F1): rebuildSkills always refuses with replay.ErrPinned,
// as initSkills' closure does once a replay bundle pinned the roster.
func newPinnedPluginsTestHandler(t *testing.T) *Handler {
	t.Helper()
	root := t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	h := &Handler{}
	h.SetPlugins(NewPlugins(reg, root, nil, func() (map[string]error, error) {
		return nil, fmt.Errorf("plugin roster is pinned to replay bundle /tmp/x.jsonl: %w", replay.ErrPinned)
	}))
	return h
}

// TestCreatePlugin_ReplayPinnedRefuses409 is F1: a rebuild refusal caused by
// an active replay pin must map to 409, not the generic 422 every other
// rebuild refusal (e.g. a seed admission failure) still gets.
func TestCreatePlugin_ReplayPinnedRefuses409(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h := newPinnedPluginsTestHandler(t)

	w := doJSON(t, h.CreatePlugin, http.MethodPost, `{"entry":"github:acme/widgets"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
}

func TestUpdatePlugin_ReplayPinnedRefuses409(t *testing.T) {
	bare, _ := newFixtureRepo(t)
	withFixedRemote(t, bare)
	h := newPinnedPluginsTestHandler(t)
	// Seed a row directly (bypassing rebuild) so UpdatePlugin has one to find.
	if err := h.plugins.reg.Put(context.Background(), pluginreg.FromEntry(mustParseEntry(t, "github:acme/widgets"))); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.UpdatePlugin(w, r, "widgets")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
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
