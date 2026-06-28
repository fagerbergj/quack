// Package bundledir embeds the shipped agent bundles and skills into the
// binary so `quack server run` works from any directory — not just a repo
// checkout. Without this, a standalone binary in a fresh project can't load
// its own agents (agent-card.json / prompt.md) or skills, which breaks the
// whole `quack init` → `quack server run` onboarding flow.
//
// Resolution prefers files on disk in cwd (so repo dev edits to agents/ or
// skills/ are picked up live without a rebuild) and falls back to the embedded
// copy (so an installed binary, run anywhere, just works).
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

// Open resolves a path under agents/ or skills/ from disk in cwd first, then
// the embedded copy. It returns a read-only file. Callers that need the whole
// tree use FS() instead.
func Open(name string) (fs.File, error) {
	if f, err := os.Open(name); err == nil {
		return f, nil
	}
	return embedded.Open(name)
}

// ReadFile resolves name (e.g. "agents/orchestrator/agent-card.json") from disk
// in cwd first, then the embedded copy.
func ReadFile(name string) ([]byte, error) {
	if b, err := os.ReadFile(name); err == nil {
		return b, nil
	}
	return embedded.ReadFile(name)
}

// Stat reports whether name exists on disk in cwd or in the embedded copy.
func Stat(name string) bool {
	if _, err := os.Stat(name); err == nil {
		return true
	}
	if _, err := fs.Stat(embedded, name); err == nil {
		return true
	}
	return false
}

// SubFS returns an fs.FS rooted at subdir (e.g. "skills"), preferring disk in
// cwd and falling back to the embedded subtree. Used by the skill toolset,
// which takes an fs.FS. A nil/missing subtree yields an empty fs.FS that errors
// on every open (the caller self-disables).
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
