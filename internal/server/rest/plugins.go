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
	"github.com/fagerbergj/quack/internal/schema"
)

// pluginUpdateBudget bounds one updates check or update-all call's total wall time;
// a var so a test can shrink it.
var pluginUpdateBudget = 30 * time.Second

// pluginConcurrency bounds how many rows' CheckUpdate/Fetch run in flight at
// once - a fixed small number, not one goroutine per row.
const pluginConcurrency = 4

// Plugins is the REST handler's boot-owned registry access.
type Plugins struct {
	reg  pluginreg.FetchRegistry
	root string
	seed []string
	// reload rebuilds the roster from the registry; an error means nothing
	// swapped, and a refused row is a failure with stage admission.
	reload func(ctx context.Context) (schema.PluginReloadReport, error)
	// mcpDeclared reports, by row name, which plugins currently declare an
	// mcp.json server - the note on the wire row.
	mcpDeclared func() map[string]bool
	// writeMu serializes every fetch/delete with the reload after it, so a
	// reload never reads a clone mid-checkout.
	writeMu sync.Mutex
}

// NewPlugins builds the handler's registry access. reload and mcpDeclared
// may be nil (no-op) for a caller not keeping the roster live.
func NewPlugins(reg pluginreg.FetchRegistry, root string, seed []string, reload func(context.Context) (schema.PluginReloadReport, error), mcpDeclared func() map[string]bool) *Plugins {
	return &Plugins{reg: reg, root: root, seed: seed, reload: reload, mcpDeclared: mcpDeclared}
}

// declaresMCP reports whether name currently declares an mcp.json server.
func (p *Plugins) declaresMCP(name string) bool {
	if p == nil || p.mcpDeclared == nil {
		return false
	}
	return p.mcpDeclared()[name]
}

// rebuild reloads the roster. A non-nil err means nothing swapped (a seed
// refusal, a registry read failure, ...); the report still says why.
func (p *Plugins) rebuild(ctx context.Context) (schema.PluginReloadReport, error) {
	if p == nil || p.reload == nil {
		return NewReloadReport(0), nil
	}
	return p.reload(ctx)
}

// NewReloadReport starts every list empty, never null, on the wire.
func NewReloadReport(gen uint64) schema.PluginReloadReport {
	return schema.PluginReloadReport{
		Generation: int64(gen),
		Agents:     schema.PluginReloadAgents{Added: []string{}, Updated: []schema.PluginReloadAgentUpdate{}, Removed: []string{}},
		Workflows:  schema.PluginReloadNames{Added: []string{}, Updated: []string{}, Removed: []string{}},
		McpServers: schema.PluginReloadServers{Started: []string{}, Reused: []string{}, Stopped: []string{}},
		Failures:   []schema.PluginReloadFailure{},
	}
}

// reloadError is a 422 carrying the reload's report next to the message.
func reloadError(w http.ResponseWriter, msg string, rep schema.PluginReloadReport) {
	writeJSON(w, http.StatusUnprocessableEntity, schema.PluginReloadError{Error: msg, Reload: rep})
}

// fetchBounded fetches one row within pluginUpdateBudget, since writeMu is
// held across it; a failure lands on the row (Error), not the response.
func (p *Plugins) fetchBounded(ctx context.Context, row pluginreg.Plugin) pluginreg.Plugin {
	ctx, cancel := context.WithTimeout(ctx, pluginUpdateBudget)
	defer cancel()
	fetched, _ := p.reg.Fetch(ctx, row)
	return fetched
}

// refusal is name's admission failure in rep, if the reload refused that row.
func refusal(rep schema.PluginReloadReport, name string) (string, bool) {
	for _, f := range rep.Failures {
		if f.Stage == schema.Admission && f.Plugin != nil && *f.Plugin == name {
			return f.Error, true
		}
	}
	return "", false
}

// allRows lists every registry row plus the embedded "quack" baseline,
// unless a real row named "quack" shadows it.
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

func pluginWire(root string, p pluginreg.Plugin, declaresMCP bool) schema.Plugin {
	w := schema.Plugin{
		Name:   p.Name,
		Entry:  p.Entry,
		Source: schema.PluginSource(p.Source),
	}
	if declaresMCP {
		w.DeclaresMcpServers = &declaresMCP
	}
	if p.Seeded {
		w.Seeded = &p.Seeded
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
// (/plugins/update, /plugins/updates, /plugins/reload) or the embedded baseline.
var reservedPluginNames = map[string]bool{
	"update": true, "updates": true, "reload": true, pluginreg.EmbeddedQuackPluginName: true,
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
		wire[i] = pluginWire(h.plugins.root, p, h.plugins.declaresMCP(p.Name))
	}
	writeJSON(w, http.StatusOK, schema.PluginList{Plugins: wire})
}

// CreatePlugin stores a github: entry (local roots stay config-only) and fetches it synchronously.
// A fetch failure still returns 201 with the row's error set.
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
	h.plugins.writeMu.Lock()
	defer h.plugins.writeMu.Unlock()
	if err := h.plugins.reg.Put(r.Context(), row); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, pluginreg.ErrNameCollision) {
			status = http.StatusConflict
		}
		errMsg(w, status, err.Error())
		return
	}
	// Put may have preserved an installed sha/fetched_at (re-POST of the same entry), so re-read
	// to report that state on a failed Fetch.
	if rows, err := h.plugins.reg.List(r.Context()); err == nil {
		if existing, ok := findPluginRow(rows, row.Name); ok {
			row = existing
		}
	}
	fetched := h.plugins.fetchBounded(r.Context(), row)
	h.writeReloaded(w, r, http.StatusCreated, fetched)
}

// writeReloaded reloads after adding or updating row: 422 when nothing swapped or row was refused.
// The row stays registered either way.
func (h *Handler) writeReloaded(w http.ResponseWriter, r *http.Request, status int, row pluginreg.Plugin) {
	rep, err := h.plugins.rebuild(r.Context())
	if err != nil {
		reloadError(w, err.Error(), rep)
		return
	}
	if msg, ok := refusal(rep, row.Name); ok {
		reloadError(w, msg, rep)
		return
	}
	wire := pluginWire(h.plugins.root, row, h.plugins.declaresMCP(row.Name))
	wire.Reload = &rep
	writeJSON(w, status, wire)
}

// DeletePlugin removes a github row and its clone. "quack" is reserved outright,
// even against a github row that shadows it; a local root is config-only (409).
func (h *Handler) DeletePlugin(w http.ResponseWriter, r *http.Request, name schema.PluginName) {
	if !h.requirePlugins(w) {
		return
	}
	if name == pluginreg.EmbeddedQuackPluginName {
		errMsg(w, http.StatusBadRequest, `"quack" is reserved for the built-in embedded plugin and cannot be removed`)
		return
	}
	h.plugins.writeMu.Lock()
	defer h.plugins.writeMu.Unlock()
	rows, err := h.plugins.reg.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if row, ok := findPluginRow(rows, name); ok && row.Source == pluginreg.SourceLocal {
		errMsg(w, http.StatusConflict, fmt.Sprintf("%q is a local root from plugins.seed; remove it from quack.yaml and restart instead", name))
		return
	}
	err = h.plugins.reg.Delete(r.Context(), name)
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
	// The delete already committed, so a failed reload is reported, not refused.
	rep, err := h.plugins.rebuild(r.Context())
	if err != nil {
		slog.Warn("plugin reload failed after delete; the previous roster keeps serving until the next reload",
			"component", "rest", "err", err)
	}
	writeJSON(w, http.StatusOK, schema.PluginDeleted{Reload: rep})
}

// ReloadPlugins rebuilds the roster from the registry: 200 with what
// changed, or 422 with the same report when nothing swapped.
func (h *Handler) ReloadPlugins(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	h.plugins.writeMu.Lock()
	defer h.plugins.writeMu.Unlock()
	rep, err := h.plugins.rebuild(r.Context())
	status := http.StatusOK
	if err != nil {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, rep)
}

// ListPluginUpdates checks every github row against its tracked ref within pluginUpdateBudget and
// pluginConcurrency; a per-row failure lands only in that row's error field.
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

// UpdatePlugin re-fetches one row against its tracked/pinned ref; a manual click,
// so it fetches regardless of CheckUpdate.
func (h *Handler) UpdatePlugin(w http.ResponseWriter, r *http.Request, name schema.PluginName) {
	if !h.requirePlugins(w) {
		return
	}
	h.plugins.writeMu.Lock()
	defer h.plugins.writeMu.Unlock()
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
	fetched := h.plugins.fetchBounded(r.Context(), row)
	h.writeReloaded(w, r, http.StatusOK, fetched)
}

// UpdateAllPlugins fetches only rows CheckUpdate reports behind; current or failed-check rows
// are reported, not fetched.
func (h *Handler) UpdateAllPlugins(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlugins(w) {
		return
	}
	h.plugins.writeMu.Lock()
	defer h.plugins.writeMu.Unlock()
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
	rep, rerr := h.plugins.rebuild(r.Context())
	if rerr != nil {
		reloadError(w, rerr.Error(), rep)
		return
	}
	wire := make([]schema.Plugin, len(results))
	for i, p := range results {
		// A refusal is reported per-row (epic: never fail the whole
		// response) - it doesn't override a check/fetch error already set.
		if msg, ok := refusal(rep, p.Name); ok && p.Error == "" {
			p.Error = msg
		}
		wire[i] = pluginWire(h.plugins.root, p, h.plugins.declaresMCP(p.Name))
	}
	writeJSON(w, http.StatusOK, schema.PluginList{Plugins: wire, Reload: &rep})
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
