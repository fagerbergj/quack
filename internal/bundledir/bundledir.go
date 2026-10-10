// Package bundledir reads agent bundles and skills from cwd, falling back to the embedded copies, so an
// installed binary works anywhere while repo edits apply without a rebuild.
package bundledir

import (
	"io/fs"
	"maps"
	"os"
	"slices"

	root "github.com/fagerbergj/quack"
)

// embedded is the agents/ + skills/ tree baked in at the repo root (embed.go), the fallback after disk.
var embedded = root.Embedded

// ReadFile resolves name (e.g. "agents/orchestrator/agent-card.json") from disk
// in cwd first, then the embedded copy.
func ReadFile(name string) ([]byte, error) {
	if b, err := os.ReadFile(name); err == nil {
		return b, nil
	}
	return embedded.ReadFile(name)
}

// SubFS returns an fs.FS rooted at subdir, disk first, then embedded. A missing subtree yields an fs.FS
// that errors on every open (the caller self-disables).
func SubFS(subdir string) fs.FS {
	if _, err := os.Stat(subdir); err == nil {
		if sub, err := fs.Sub(os.DirFS("."), subdir); err == nil {
			return sub
		}
	}
	sub, err := fs.Sub(embedded, subdir)
	if err != nil {
		return errFS{}
	}
	return sub
}

// UnionDirNames lists dir under subdir from both disk and embedded, deduped and sorted: SubFS prefers disk
// wholesale, so a partial bind-mount would hide shipped entries.
func UnionDirNames(subdir, dir string) []string {
	seen := map[string]bool{}
	read := func(fsys fs.FS) {
		des, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return
		}
		for _, de := range des {
			seen[de.Name()] = true
		}
	}
	read(os.DirFS(subdir))
	if sub, err := fs.Sub(embedded, subdir); err == nil {
		read(sub)
	}
	names := slices.Collect(maps.Keys(seen))
	slices.Sort(names)
	return names
}

type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
