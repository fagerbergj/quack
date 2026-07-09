// Package workspace is the isolation boundary every filesystem/git tool
// resolves paths through: one configured root, with a per-user jail under it
// (<root>/<user_id>/) that nothing outside can read, write, or even stat. It is
// intentionally dependency-free (stdlib only) — internal/tools/fs.go (and the
// later git tools) build on it, but the boundary itself never needs anything
// beyond os/path/filepath.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is the uniform rejection for any path that would resolve outside
// the caller's jail — a `..` escape, an absolute path, or a symlink pointing
// outside. Every rejection reason collapses to this one error (and message) by
// design: a model gets one thing to learn, not a taxonomy of jail failures.
var ErrEscape = errors.New("path escapes your workspace")

// Jail is one configured workspace root; per-user boundaries are derived from
// it at resolve time (Resolve("alice", …) and Resolve("bob", …) never see each
// other's files), so a single Jail serves every user.
type Jail struct {
	// root is the absolute, symlink-resolved workspace root (e.g. /workspace).
	root string
}

// NewJail builds a Jail rooted at root (created if missing) and returns it with
// root canonicalized (absolute, symlinks resolved) so every later containment
// check compares real paths, not aliases of the same directory.
func NewJail(root string) (*Jail, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace: root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create root %q: %w", abs, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root symlinks %q: %w", abs, err)
	}
	return &Jail{root: real}, nil
}

// Root returns the jail's canonical root directory (for logging/diagnostics —
// tools should use Resolve/UserRoot, never construct paths against this
// directly).
func (j *Jail) Root() string { return j.root }

// UserRoot returns the (unresolved-for-symlinks) jail directory for userID,
// joined under the workspace root. It may not exist yet — callers that need it
// to exist (e.g. write_file) create it themselves.
func (j *Jail) UserRoot(userID string) string {
	return filepath.Join(j.root, userID)
}

// Resolve is the ONE path-resolution function every filesystem/git tool uses:
// it joins relPath under <root>/<userID>/, cleans it, resolves symlinks on the
// deepest existing ancestor, and verifies the result is prefix-contained in the
// user's jail. Absolute relPath, `..` escapes, and symlinks pointing outside
// the jail all fail identically with ErrEscape — "no exceptions" per the
// isolation design. A symlink that stays inside the jail resolves and works
// normally.
func (j *Jail) Resolve(userID, relPath string) (string, error) {
	if strings.TrimSpace(userID) == "" {
		return "", fmt.Errorf("workspace: user id is empty")
	}
	if filepath.IsAbs(relPath) {
		return "", ErrEscape
	}
	userRoot := filepath.Join(j.root, userID)
	joined := filepath.Join(userRoot, relPath)
	if !withinRoot(userRoot, joined) {
		return "", ErrEscape
	}

	realUserRoot, err := resolveDeepestExisting(userRoot)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve user root: %w", err)
	}
	real, err := resolveDeepestExisting(joined)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve path: %w", err)
	}
	if !withinRoot(realUserRoot, real) {
		return "", ErrEscape
	}
	return real, nil
}

// withinRoot reports whether path is root itself or a descendant of it. Both
// arguments must already be Clean'd absolute paths (filepath.Join guarantees
// this).
func withinRoot(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolveDeepestExisting resolves symlinks on the deepest existing ancestor of
// p (p itself, if it exists) and rejoins any trailing path components that
// don't exist yet (e.g. a file about to be created by write_file). p must
// already be an absolute, Clean'd path.
func resolveDeepestExisting(p string) (string, error) {
	cur := p
	var trailing []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(trailing) - 1; i >= 0; i-- {
				real = filepath.Join(real, trailing[i])
			}
			return real, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root and nothing exists on the path at all —
			// there are no symlinks to resolve; the cleaned path is already real.
			return p, nil
		}
		trailing = append(trailing, filepath.Base(cur))
		cur = parent
	}
}
