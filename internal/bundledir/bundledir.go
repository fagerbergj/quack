// Package bundledir resolves agent bundles and skills from files in cwd, falling back to the
// embedded copies, so an installed binary works from any directory (a fresh project's `quack
// init` → `server run` needs its own agents). Disk preference picks up repo dev edits without a rebuild.
package bundledir

import (
	"io/fs"
	"os"
	"path"

	root "github.com/fagerbergj/quack"
)

// embedded is the agents/ + skills/ tree baked in at the repo root (see
// embed.go). Disk-in-cwd is tried first for live repo edits; this is the
// fallback that makes an installed binary work from any directory.
var embedded = root.Embedded

// ReadFile resolves name (e.g. "agents/orchestrator/agent-card.json") from disk
// in cwd first, then the embedded copy.
func ReadFile(name string) ([]byte, error) {
	if b, err := os.ReadFile(name); err == nil {
		return b, nil
	}
	return embedded.ReadFile(name)
}

// SubFS returns an fs.FS rooted at subdir (e.g. "skills"), disk in cwd first,
// then the embedded subtree. A missing subtree yields an empty fs.FS that
// errors on every open (the caller self-disables).
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

type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// PathJoin joins bundle-relative path elements with forward slashes (works for
// both embed.FS and os.DirFS on every platform).
func PathJoin(elems ...string) string { return path.Join(elems...) }
