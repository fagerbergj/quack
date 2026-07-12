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

// ErrInvalidUserID rejects a userID that cannot safely name one directory
// component under the workspace root. Deliberately DISTINCT from ErrEscape:
// a bad userID is a caller bug or a misconfigured identity source (an
// operator's problem to fix), not a model-chosen path (which the model can
// learn from and correct).
var ErrInvalidUserID = errors.New("workspace: invalid user id")

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
// to exist (e.g. write_file) create it themselves. A userID that fails
// validateUserID returns ErrInvalidUserID.
func (j *Jail) UserRoot(userID string) (string, error) {
	if err := validateUserID(userID); err != nil {
		return "", err
	}
	return filepath.Join(j.root, userID), nil
}

// homeDirName is the dot-prefixed sibling directory HomeDir creates under a
// user's jail root — dot-prefixed so it reads unmistakably as infrastructure,
// not a cloned repo, in a directory listing.
const homeDirName = ".quack-home"

// HomeDir returns (creating it if necessary) userID's dedicated $HOME for
// spawned child processes (run_command, checks, git — see workspace.Caps.
// HomeDir and internal/workspace/exec.go). It is a SIBLING of the user's
// cloned repos under <root>/<userID>/, never nested inside one — the fix for
// a live bug where HOME was pinned to a coding task's own cwd (the target
// repo itself), so `npm ci` wrote its cache directly into the repo tree and a
// later `git_commit`'s `add_all` swept up thousands of cache files alongside
// the real change. Created with 0o700 (a user's toolchain cache/config is not
// world- or other-user-readable).
func (j *Jail) HomeDir(userID string) (string, error) {
	userRoot, err := j.UserRoot(userID)
	if err != nil {
		return "", err
	}
	home := filepath.Join(userRoot, homeDirName)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("workspace: create home dir %q: %w", home, err)
	}
	return home, nil
}

// validateUserID is the jail-boundary guard shared by UserRoot and Resolve:
// a userID must name exactly ONE directory component directly under the
// workspace root, because Resolve joins it into the path RAW — an
// attacker-influenced identity ("../other", "a/b", an absolute path) would
// otherwise relocate the jail root itself, and the containment check would
// then verify against the WRONG root. The rule is separator/dot-traversal
// based, NOT an alphanumeric allowlist: real OIDC subjects like
// "auth0|abc123" or "user@example.com" must pass.
func validateUserID(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return ErrInvalidUserID
	}
	if userID == "." || userID == ".." {
		return ErrInvalidUserID
	}
	if strings.ContainsRune(userID, '/') || strings.ContainsRune(userID, os.PathSeparator) {
		return ErrInvalidUserID
	}
	if filepath.Clean(userID) != userID {
		return ErrInvalidUserID
	}
	return nil
}

// Resolve is the ONE path-resolution function every filesystem/git tool uses:
// it joins relPath under <root>/<userID>/, cleans it, resolves symlinks on the
// deepest existing ancestor, and verifies the result is prefix-contained in the
// user's jail. Absolute relPath, `..` escapes, and symlinks pointing outside
// the jail all fail identically with ErrEscape — "no exceptions" per the
// isolation design. A symlink that stays inside the jail resolves and works
// normally. userID is guarded first (validateUserID → ErrInvalidUserID): it
// must be a single path component, or the jail root itself would relocate.
func (j *Jail) Resolve(userID, relPath string) (string, error) {
	userRoot, err := j.UserRoot(userID)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relPath) {
		return "", ErrEscape
	}
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
