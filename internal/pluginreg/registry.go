package pluginreg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

	SHA       string    `json:"sha,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// FromEntry builds an unfetched row from a parsed Entry.
func FromEntry(e Entry) Plugin {
	return Plugin{
		Entry: e.Raw, Source: e.Source, Name: e.Name(),
		Owner: e.Owner, Repo: e.Repo, Ref: e.Ref, Path: e.Path,
	}
}

// Root is the plugin's resolved skills root: for a local entry, the entry
// itself; for a github entry, <registryRoot>/<name>/repo/<path>.
func (p Plugin) Root(registryRoot string) string {
	if p.Source == SourceLocal {
		return p.Entry
	}
	return filepath.Join(CloneDir(registryRoot, p.Name), p.Path)
}

// CloneDir is where a github plugin's full clone lives under the registry root.
func CloneDir(registryRoot, name string) string {
	return filepath.Join(registryRoot, name, "repo")
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
}

func NewFSRegistry(root string) *FSRegistry {
	return &FSRegistry{root: root}
}

func (r *FSRegistry) Root() string { return r.root }

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
	return out, nil
}

func (r *FSRegistry) readRow(name string) (Plugin, error) {
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

func (r *FSRegistry) Put(ctx context.Context, p Plugin) error {
	if p.Name == "" {
		return fmt.Errorf("plugin row has no name")
	}
	dir := filepath.Join(r.root, p.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := rowPath(r.root, p.Name) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, rowPath(r.root, p.Name))
}

func (r *FSRegistry) Delete(ctx context.Context, name string) error {
	return os.RemoveAll(filepath.Join(r.root, name))
}
