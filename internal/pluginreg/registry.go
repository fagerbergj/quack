package pluginreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrInvalidName is validName's sentinel (empty, ".", "..", or a separator).
// ErrNameCollision is Put's sentinel: name is already registered under a
// different identity - REST maps it to 409, everything else to 500.
var (
	ErrInvalidName   = errors.New("invalid plugin name")
	ErrNameCollision = errors.New("plugin name already registered under a different entry")
)

// Plugin is one registry row: an entry plus its resolved fields and fetch state.
type Plugin struct {
	Entry  string `json:"entry"`
	Source string `json:"source"` // SourceGitHub or SourceLocal
	Name   string `json:"name"`
	Owner  string `json:"owner,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`

	SHA string `json:"sha,omitempty"`
	// FetchedAt is a pointer so "never fetched" serializes as an absent field
	// instead of the zero time.
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// FromEntry builds an unfetched row from a parsed Entry.
func FromEntry(e Entry) Plugin {
	return Plugin{
		Entry: e.Raw, Source: e.Source, Name: e.Name(),
		Owner: e.Owner, Repo: e.Repo, Ref: e.Ref, Path: e.Path,
	}
}

// Root: local entry = the path itself; github = <registryRoot>/<name>/repo/<path>.
// Rows are trusted off disk without re-parsing, so a Path escaping the clone
// falls back to the clone root instead of serving outside it.
func (p Plugin) Root(registryRoot string) string {
	if p.Source == SourceLocal {
		return p.Entry
	}
	base := CloneDir(registryRoot, p.Name)
	if p.Path == "" {
		return base
	}
	if joined, err := containedPath(base, p.Path); err == nil {
		return joined
	}
	return base
}

// containedPath joins rel under base and refuses any result that escapes it
// (mirrors internal/plugin's containedPath).
func containedPath(base, rel string) (string, error) {
	p := filepath.Clean(filepath.Join(base, rel))
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes %q", rel, base)
	}
	return p, nil
}

// CloneDir is where a github plugin's full clone lives under the registry root.
func CloneDir(registryRoot, name string) string {
	return filepath.Join(registryRoot, name, "repo")
}

// EmbeddedQuackPluginName is the go:embedded skill bundle's fixed row name -
// reserved, shadowable by a same-named github row, never Put or deleted.
const EmbeddedQuackPluginName = "quack"

// EmbeddedQuackPlugin is the embedded bundle's in-memory row (#1427 P1),
// shared by boot and the REST listing so both include it identically.
func EmbeddedQuackPlugin() Plugin {
	return Plugin{Name: EmbeddedQuackPluginName, Source: SourceEmbedded}
}

// OrderBySeed reorders rows to match seed's listed order (bare-name
// resolution is "first in merge order wins", #1427 F2) - a row not in seed
// (added via the UI/REST, P2) sorts after, in List's name order.
func OrderBySeed(seed []string, rows []Plugin) []Plugin {
	byName := make(map[string]Plugin, len(rows))
	for _, p := range rows {
		byName[p.Name] = p
	}
	out := make([]Plugin, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, s := range seed {
		e, err := ParseEntry(s) // config.validatePlugins already checked every entry parses
		if err != nil {
			continue
		}
		if p, ok := byName[e.Name()]; ok && !seen[e.Name()] {
			out = append(out, p)
			seen[e.Name()] = true
		}
	}
	for _, p := range rows {
		if !seen[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

func rowPath(registryRoot, name string) string {
	return filepath.Join(registryRoot, name, "entry.json")
}

// Registry stores plugin rows. Filesystem is the only backend P0 ships;
// sqlite/postgres land in P3 behind the same interface.
type Registry interface {
	List(ctx context.Context) ([]Plugin, error)
	Put(ctx context.Context, p Plugin) error
	Delete(ctx context.Context, name string) error
}

// FSRegistry stores each row at <root>/<name>/entry.json, with the plugin's
// clone (if any) as a sibling at <root>/<name>/repo.
type FSRegistry struct {
	root string
	// ponytail: mu only serializes writes within one process; a second quack
	// process sharing root can still race. Add a lock file if that happens.
	mu sync.Mutex
}

func NewFSRegistry(root string) *FSRegistry {
	return &FSRegistry{root: root}
}

// List returns every row, sorted by name.
func (r *FSRegistry) List(ctx context.Context) ([]Plugin, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Plugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := r.readRow(e.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue // dir with no entry.json (e.g. a stray clone-only dir)
			}
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *FSRegistry) readRow(name string) (Plugin, error) {
	if err := validName(name); err != nil {
		return Plugin{}, err
	}
	b, err := os.ReadFile(rowPath(r.root, name))
	if err != nil {
		return Plugin{}, err
	}
	var p Plugin
	if err := json.Unmarshal(b, &p); err != nil {
		return Plugin{}, fmt.Errorf("parse %s: %w", rowPath(r.root, name), err)
	}
	return p, nil
}

// samePlugin is Put's collision identity: source+owner/repo for github (a
// pin move like @v1 -> @v2 is a legal update, not a collision), the raw
// entry for local. Empty Owner/Repo derives from Entry instead (#1430).
func samePlugin(a, b Plugin) bool {
	if a.Source != b.Source {
		return false
	}
	if a.Source == SourceGitHub {
		ao, ar := githubIdentity(a)
		bo, br := githubIdentity(b)
		// Both sides unresolvable (empty owner/repo, unparsable entry) is
		// never a match - two blank identities are not "the same" plugin.
		if ao == "" && ar == "" {
			return false
		}
		return ao == bo && ar == br
	}
	return a.Entry == b.Entry
}

func githubIdentity(p Plugin) (owner, repo string) {
	if p.Owner != "" || p.Repo != "" {
		return p.Owner, p.Repo
	}
	if e, err := ParseEntry(p.Entry); err == nil {
		return e.Owner, e.Repo
	}
	return "", ""
}

// SameIdentity reports whether a and b identify the same plugin (samePlugin) -
// exported for seedRegistry's seed/disk name-collision check (#1430 carry-over).
func SameIdentity(a, b Plugin) bool { return samePlugin(a, b) }

// Put writes p's row, replacing any row identifying the SAME plugin
// (samePlugin). A different plugin under an already-registered name (e.g.
// two repos both named "widgets") is a collision, rejected outright.
func (r *FSRegistry) Put(ctx context.Context, p Plugin) error {
	if err := validName(p.Name); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, err := r.readRow(p.Name); err == nil {
		if !samePlugin(existing, p) {
			return fmt.Errorf("%w: plugin %q is already registered from %q, not %q", ErrNameCollision, p.Name, existing.Entry, p.Entry)
		}
		// A re-Put of the same identity with no sha yet (REST's create/
		// re-create path, before its own Fetch runs) must not wipe the last
		// good clone's sha/fetched_at - only Fetch may move those forward.
		if p.SHA == "" && p.FetchedAt == nil {
			p.SHA, p.FetchedAt = existing.SHA, existing.FetchedAt
		}
	}
	// Plugin is all strings/*time.Time - MarshalIndent on it cannot fail.
	b, _ := json.MarshalIndent(p, "", "  ")
	dir := filepath.Join(r.root, p.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "entry-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return werr
		}
		return cerr
	}
	return os.Rename(tmp, rowPath(r.root, p.Name))
}

// Delete removes name's row and clone. A name with no row is an error
// wrapping os.ErrNotExist.
func (r *FSRegistry) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.root, name)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("plugin %q: %w", name, os.ErrNotExist)
		}
		return err
	}
	return os.RemoveAll(dir)
}
