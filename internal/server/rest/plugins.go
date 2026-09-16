package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/schema"
)

// pluginUpdateBudget bounds one GET /plugins/updates or POST /plugins/update
// call's total wall time, regardless of row count - var so a test can shrink
// it to prove the budget actually expires, instead of waiting 30s.
var pluginUpdateBudget = 30 * time.Second

// pluginConcurrency bounds how many rows' CheckUpdate/Fetch run in flight at
// once - a fixed small number, not one goroutine per row.
const pluginConcurrency = 4

// pluginRegistry is the subset of *pluginreg.FSRegistry these handlers use -
// narrow enough that a test's own FSRegistry (against a fixture root) needs
// no mock.
type pluginRegistry interface {
	pluginreg.Registry
	Fetch(ctx context.Context, p pluginreg.Plugin) (pluginreg.Plugin, error)
	CheckUpdate(ctx context.Context, p pluginreg.Plugin) (behind bool, remoteSHA string, err error)
}

// Plugins is the REST handler's boot-owned registry access (epic #1427 P2).
type Plugins struct {
	reg  pluginRegistry
	root string
	seed []string
	// rebuildSkills swaps in a fresh roster; refusals names non-seed rows
	// dropped this pass - the same per-row admission boot uses (#1430).
	rebuildSkills func() (refusals map[string]error, err error)
}

// NewPlugins builds the handler's registry access. rebuildSkills may be nil
// (no-op) for a caller that doesn't need the roster kept live, e.g. a test.
func NewPlugins(reg pluginRegistry, root string, seed []string, rebuildSkills func() (map[string]error, error)) *Plugins {
	return &Plugins{reg: reg, root: root, seed: seed, rebuildSkills: rebuildSkills}
}

// rebuild re-resolves the native skill roster. A non-nil err is fatal (a
// seed refusal, or a registry read failure); refusals[name] is set when
// name was refused and dropped - the caller decides what that means.
func (p *Plugins) rebuild() (refusals map[string]error, err error) {
	if p == nil || p.rebuildSkills == nil {
		return nil, nil
	}
	return p.rebuildSkills()
}

// rebuildStatus maps a rebuild error to its HTTP status - replay.ErrPinned
// (#1427 P4 F1) is a 409, everything else stays the existing 422.
func rebuildStatus(err error) int {
	if errors.Is(err, replay.ErrPinned) {
		return http.StatusConflict
	}
	return http.StatusUnprocessableEntity
}

// rebuildOrWarn is DeletePlugin's rebuild call: the delete already
// committed, so a rebuild failure is logged, not surfaced - there is nothing
// left to refuse.
func (p *Plugins) rebuildOrWarn() {
	if _, err := p.rebuild(); err != nil {
		slog.Warn("plugin roster rebuild failed after delete; native agents may serve a stale skill list until restart",
			"component", "rest", "err", err)
	}
}

// allRows lists every registry row plus the embedded "quack" baseline -
// unless a real row already named "quack" shadows it (epic #1427 S2):
// one row named quack, never two.
func (p *Plugins) allRows(ctx context.Context) ([]pluginreg.Plugin, error) {
	rows, err := p.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	rows = pluginreg.OrderBySeed(p.seed, rows)
	for _, row := range rows {
		if row.Name == pluginreg.EmbeddedQuackPluginName {
			return rows, nil
		}
	}
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

// reservedPluginNames collide with a fixed REST path segment
// (/plugins/update, /plugins/updates) or the embedded baseline.
var reservedPluginNames = map[string]bool{
	"update": true, "updates": true, pluginreg.EmbeddedQuackPluginName: true,
}

func reservedPluginNameError(name string) string {
	if reservedPluginNames[name] {
		return fmt.Sprintf("%q is a reserved plugin name", name)
	}
	return ""
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

// CreatePlugin parses entry (github: only - a local root stays config-only),
// stores the row, then fetches it synchronously. A fetch failure still
// returns 201 with the row (error set), not a failed add.
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
	if entry.Source != pluginreg.SourceGitHub {
		errMsg(w, http.StatusBadRequest, `entry must be "github:owner/repo[@ref][#path]"`)
		return
	}
	if msg := reservedPluginNameError(entry.Name()); msg != "" {
		errMsg(w, http.StatusBadRequest, msg)
		return
	}
	row := pluginreg.FromEntry(entry)
	if err := h.plugins.reg.Put(r.Context(), row); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, pluginreg.ErrNameCollision) {
			status = http.StatusConflict
		}
		errMsg(w, status, err.Error())
		return
	}
	// Put may have preserved an already-installed sha/fetched_at (a re-POST
	// of the same entry, severe#2) - re-read so a failed Fetch below reports
	// that preserved state, not the blank row this func built.
	if rows, err := h.plugins.reg.List(r.Context()); err == nil {
		if existing, ok := findPluginRow(rows, row.Name); ok {
			row = existing
		}
	}
	fetched, _ := h.plugins.reg.Fetch(r.Context(), row) // fetch failure lands on the row (Error), not the response
	refusals, rerr := h.plugins.rebuild()
	if rerr != nil {
		errMsg(w, rebuildStatus(rerr), rerr.Error())
		return
	}
	if refused, ok := refusals[fetched.Name]; ok {
		// admitPlugins already persisted refused's message onto THIS row -
		// mirror it here rather than re-deriving or stamping the wrong row.
		fetched.Error = refused.Error()
		errMsg(w, http.StatusUnprocessableEntity, refused.Error())
		return
	}
	writeJSON(w, http.StatusCreated, pluginWire(h.plugins.root, fetched))
}

// DeletePlugin removes a row and its clone. "quack" is reserved outright,
// even against a github row that shadows it (unshadowing is out of scope).
func (h *Handler) DeletePlugin(w http.ResponseWriter, r *http.Request, name schema.PluginName) {
	if !h.requirePlugins(w) {
		return
	}
	if name == pluginreg.EmbeddedQuackPluginName {
		errMsg(w, http.StatusBadRequest, `"quack" is reserved for the built-in embedded plugin and cannot be removed`)
		return
	}
	err := h.plugins.reg.Delete(r.Context(), name)
	if errors.Is(err, pluginreg.ErrInvalidName) {
		errMsg(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	h.plugins.rebuildOrWarn()
	w.WriteHeader(http.StatusNoContent)
}

// ListPluginUpdates checks every github-sourced row against its tracked ref,
// bounded to pluginUpdateBudget total and pluginConcurrency in flight. A
// per-row check failure lands in that row's error field only.
func (h *Handler) ListPluginUpdates(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pluginUpdateBudget)
	defer cancel()
	updates := mapConcurrently(ctx, githubRows(rows), func(ctx context.Context, p pluginreg.Plugin) schema.PluginUpdate {
		u := schema.PluginUpdate{Name: p.Name}
		if p.SHA != "" {
			u.InstalledSha = &p.SHA
		}
		behind, remoteSHA, err := h.plugins.reg.CheckUpdate(ctx, p)
		if err != nil {
			msg := err.Error()
			u.Error = &msg
			return u
		}
		u.Behind = behind
		if remoteSHA != "" {
			u.RemoteSha = &remoteSHA
		}
		return u
	})
	writeJSON(w, http.StatusOK, schema.PluginUpdateList{Updates: updates})
}

// UpdatePlugin re-fetches one row against its tracked/pinned ref - a manual,
// per-row click, so unlike UpdateAllPlugins it fetches regardless of
// CheckUpdate's answer.
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
	refusals, rerr := h.plugins.rebuild()
	if rerr != nil {
		errMsg(w, rebuildStatus(rerr), rerr.Error())
		return
	}
	if refused, ok := refusals[fetched.Name]; ok {
		fetched.Error = refused.Error()
		errMsg(w, http.StatusUnprocessableEntity, refused.Error())
		return
	}
	writeJSON(w, http.StatusOK, pluginWire(h.plugins.root, fetched))
}

// UpdateAllPlugins fetches only the rows CheckUpdate reports behind (epic:
// "(all behind)") - a row already current, or one whose check itself
// failed, is reported but not fetched. Bounded like ListPluginUpdates.
func (h *Handler) UpdateAllPlugins(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pluginUpdateBudget)
	defer cancel()
	results := mapConcurrently(ctx, githubRows(rows), func(ctx context.Context, p pluginreg.Plugin) pluginreg.Plugin {
		behind, _, err := h.plugins.reg.CheckUpdate(ctx, p)
		if err != nil {
			p.Error = err.Error()
			return p
		}
		if !behind {
			return p
		}
		fetched, _ := h.plugins.reg.Fetch(ctx, p) // fetch failure lands on the row (Error)
		return fetched
	})
	refusals, rerr := h.plugins.rebuild()
	if rerr != nil {
		errMsg(w, rebuildStatus(rerr), rerr.Error())
		return
	}
	wire := make([]schema.Plugin, len(results))
	for i, p := range results {
		// A refusal is reported per-row (epic: never fail the whole
		// response) - it doesn't override a check/fetch error already set.
		if refused, ok := refusals[p.Name]; ok && p.Error == "" {
			p.Error = refused.Error()
		}
		wire[i] = pluginWire(h.plugins.root, p)
	}
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

func githubRows(rows []pluginreg.Plugin) []pluginreg.Plugin {
	out := make([]pluginreg.Plugin, 0, len(rows))
	for _, p := range rows {
		if p.Source == pluginreg.SourceGitHub {
			out = append(out, p)
		}
	}
	return out
}

// mapConcurrently runs fn over rows bounded to pluginConcurrency in flight,
// preserving rows' order in the result regardless of completion order.
func mapConcurrently[T any](ctx context.Context, rows []pluginreg.Plugin, fn func(context.Context, pluginreg.Plugin) T) []T {
	out := make([]T, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, pluginConcurrency)
	for i, p := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p pluginreg.Plugin) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(ctx, p)
		}(i, p)
	}
	wg.Wait()
	return out
}
