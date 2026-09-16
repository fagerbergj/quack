package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
)

// pluginRegistry is the subset of *pluginreg.FSRegistry these handlers use -
// narrow enough that a test's own FSRegistry (against a fixture root) needs
// no mock.
type pluginRegistry interface {
	List(ctx context.Context) ([]pluginreg.Plugin, error)
	Put(ctx context.Context, p pluginreg.Plugin) error
	Delete(ctx context.Context, name string) error
	Fetch(ctx context.Context, p pluginreg.Plugin) (pluginreg.Plugin, error)
	CheckUpdate(ctx context.Context, p pluginreg.Plugin) (behind bool, remoteSHA string, err error)
}

// Plugins is the REST handler's boot-owned access to the dynamic plugin
// registry (epic #1427 P2): the store, the registry root (Plugin.Root's wire
// field), plugins.seed (row display order), and the hook that rebuilds
// native agents' skill roster after a row-set or sha change.
type Plugins struct {
	reg           pluginRegistry
	root          string
	seed          []string
	rebuildSkills func() error
}

// NewPlugins builds the handler's registry access. rebuildSkills may be nil
// (no-op) for a caller that doesn't need the roster kept live, e.g. a test.
func NewPlugins(reg pluginRegistry, root string, seed []string, rebuildSkills func() error) *Plugins {
	return &Plugins{reg: reg, root: root, seed: seed, rebuildSkills: rebuildSkills}
}

// rebuild re-resolves the native skill roster after a row/sha change. A
// failure is logged, not returned - the REST call that triggered it already
// succeeded and persisted; the roster catches up on the next successful call
// or restart, same fail-open posture as every other boot-time skill resolve.
func (p *Plugins) rebuild() {
	if p == nil || p.rebuildSkills == nil {
		return
	}
	if err := p.rebuildSkills(); err != nil {
		slog.Warn("plugin roster rebuild failed; native agents may serve a stale skill list until restart",
			"component", "rest", "err", err)
	}
}

// allRows lists every registry row in seed order, plus the embedded "quack"
// baseline (#1427 P2 scope: "Include the embedded quack row").
func (p *Plugins) allRows(ctx context.Context) ([]pluginreg.Plugin, error) {
	rows, err := p.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	rows = pluginreg.OrderBySeed(p.seed, rows)
	return append(rows, pluginreg.EmbeddedQuackPlugin()), nil
}

func pluginWire(root string, p pluginreg.Plugin) schema.Plugin {
	w := schema.Plugin{
		Name:   p.Name,
		Entry:  p.Entry,
		Source: schema.PluginSource(p.Source),
	}
	if p.Owner != "" {
		w.Owner = &p.Owner
	}
	if p.Repo != "" {
		w.Repo = &p.Repo
	}
	if p.Ref != "" {
		w.Ref = &p.Ref
	}
	if p.Path != "" {
		w.Path = &p.Path
	}
	if p.SHA != "" {
		w.InstalledSha = &p.SHA
	}
	if p.FetchedAt != nil {
		w.FetchedAt = p.FetchedAt
	}
	if p.Error != "" {
		w.Error = &p.Error
	}
	if p.Source != pluginreg.SourceEmbedded {
		r := p.Root(root)
		w.Root = &r
	}
	return w
}

// requirePlugins 500s with a clear message instead of a nil-pointer panic -
// only reachable if a deployment's boot wiring ever omits SetPlugins.
func (h *Handler) requirePlugins(w http.ResponseWriter) bool {
	if h.plugins != nil {
		return true
	}
	errMsg(w, http.StatusInternalServerError, "plugin registry is not configured on this server")
	return false
}

// ListPlugins serves every registered plugin, embedded baseline included.
func (h *Handler) ListPlugins(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.allRows(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	wire := make([]schema.Plugin, len(rows))
	for i, p := range rows {
		wire[i] = pluginWire(h.plugins.root, p)
	}
	writeJSON(w, http.StatusOK, schema.PluginList{Plugins: wire})
}

// CreatePlugin parses entry, stores the row, then fetches it synchronously.
// A fetch failure still returns 201 with the row (error set) - the add
// itself succeeded, the UI shows the fetch problem.
func (h *Handler) CreatePlugin(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	var body schema.CreatePluginBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		errMsg(w, http.StatusBadRequest, "malformed request body")
		return
	}
	entry, err := pluginreg.ParseEntry(body.Entry)
	if err != nil {
		errMsg(w, http.StatusBadRequest, err.Error())
		return
	}
	row := pluginreg.FromEntry(entry)
	if err := h.plugins.reg.Put(r.Context(), row); err != nil {
		errMsg(w, http.StatusConflict, err.Error())
		return
	}
	fetched, _ := h.plugins.reg.Fetch(r.Context(), row) // fetch failure lands on the row (Error), not the response
	h.plugins.rebuild()
	writeJSON(w, http.StatusCreated, pluginWire(h.plugins.root, fetched))
}

// DeletePlugin removes a row and its clone. The embedded "quack" baseline
// has no row and can never be deleted through this API.
func (h *Handler) DeletePlugin(w http.ResponseWriter, r *http.Request, name schema.PluginName) {
	if !h.requirePlugins(w) {
		return
	}
	// "quack" is reserved outright, not just the embedded row's own name - a
	// github plugin can shadow it by name (epic #1427), and unshadowing it
	// through this route is out of scope for P2 (ponytail: revisit if that's needed).
	if name == pluginreg.EmbeddedQuackPlugin().Name {
		errMsg(w, http.StatusBadRequest, `"quack" is reserved for the built-in embedded plugin and cannot be removed`)
		return
	}
	err := h.plugins.reg.Delete(r.Context(), name)
	if errors.Is(err, os.ErrNotExist) {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	h.plugins.rebuild()
	w.WriteHeader(http.StatusNoContent)
}

// ListPluginUpdates checks every github-sourced row against its tracked ref.
// A per-row check failure lands in that row's error field only.
func (h *Handler) ListPluginUpdates(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	updates := make([]schema.PluginUpdate, 0, len(rows))
	for _, p := range rows {
		if p.Source != pluginreg.SourceGitHub {
			continue
		}
		u := schema.PluginUpdate{Name: p.Name}
		if p.SHA != "" {
			u.InstalledSha = &p.SHA
		}
		behind, remoteSHA, err := h.plugins.reg.CheckUpdate(r.Context(), p)
		if err != nil {
			msg := err.Error()
			u.Error = &msg
		} else {
			u.Behind = behind
			if remoteSHA != "" {
				u.RemoteSha = &remoteSHA
			}
		}
		updates = append(updates, u)
	}
	writeJSON(w, http.StatusOK, schema.PluginUpdateList{Updates: updates})
}

// UpdatePlugin re-fetches one row against its tracked/pinned ref.
func (h *Handler) UpdatePlugin(w http.ResponseWriter, r *http.Request, name schema.PluginName) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	row, ok := findPluginRow(rows, name)
	if !ok {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	fetched, _ := h.plugins.reg.Fetch(r.Context(), row) // fetch failure lands on the row (Error)
	h.plugins.rebuild()
	writeJSON(w, http.StatusOK, pluginWire(h.plugins.root, fetched))
}

// UpdateAllPlugins re-fetches every github-sourced row.
func (h *Handler) UpdateAllPlugins(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	wire := make([]schema.Plugin, 0, len(rows))
	for _, p := range rows {
		if p.Source != pluginreg.SourceGitHub {
			continue
		}
		fetched, _ := h.plugins.reg.Fetch(r.Context(), p) // fetch failure lands on the row (Error)
		wire = append(wire, pluginWire(h.plugins.root, fetched))
	}
	h.plugins.rebuild()
	writeJSON(w, http.StatusOK, schema.PluginList{Plugins: wire})
}

func findPluginRow(rows []pluginreg.Plugin, name string) (pluginreg.Plugin, bool) {
	for _, p := range rows {
		if p.Name == name {
			return p, true
		}
	}
	return pluginreg.Plugin{}, false
}
