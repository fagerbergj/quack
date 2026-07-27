// Package workspace is the isolation boundary every filesystem/git tool
// resolves paths through: one configured root, with a per-user jail under it
// (<root>/<user_id>/) that nothing outside can read, write, or even stat. It is
// intentionally dependency-free (stdlib only) - internal/tools/fs.go (and the
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
// the caller's jail - a `..` escape, an absolute path, or a symlink pointing
// outside. Every rejection reason collapses to this one error (and message) by
// design: a model gets one thing to learn, not a taxonomy of jail failures.
var ErrEscape = errors.New("path escapes your workspace")

// ErrInvalidUserID rejects a userID that cannot safely name one directory
// component under the workspace root. Deliberately DISTINCT from ErrEscape:
// a bad userID is a caller bug or a misconfigured identity source (an
// operator's problem to fix), not a model-chosen path (which the model can
// learn from and correct).
var ErrInvalidUserID = errors.New("workspace: invalid user id")

// ErrInvalidChatID rejects a chatID that cannot safely name one directory
// component under a user's jail (the per-chat scope segment - see Resolve). A
// chat id is a system-generated UUID, but it is treated as UNTRUSTED here: the
// SAME single-component rule userID obeys (no separator, no `..`) is what keeps
// a crafted id from relocating the scope root and defeating containment. An
// EMPTY chatID is NOT an error - it means "no per-chat scope", falling back to
// the per-user root (backward compatible); only a NON-empty id that fails the
// component rule returns this.
var ErrInvalidChatID = errors.New("workspace: invalid chat id")

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

// Root returns the jail's canonical root directory (for logging/diagnostics -
// tools should use Resolve/UserRoot, never construct paths against this
// directly).
func (j *Jail) Root() string { return j.root }

// UserRoot returns the (unresolved-for-symlinks) jail directory for userID,
// joined under the workspace root. It may not exist yet - callers that need it
// to exist (e.g. write_file) create it themselves. A userID that fails
// validateUserID returns ErrInvalidUserID.
func (j *Jail) UserRoot(userID string) (string, error) {
	if err := validateUserID(userID); err != nil {
		return "", err
	}
	return filepath.Join(j.root, userID), nil
}

// homeDirName is the dot-prefixed sibling directory HomeDir creates under a
// user's jail root - dot-prefixed so it reads unmistakably as infrastructure,
// not a cloned repo, in a directory listing.
const homeDirName = ".quack-home"

// HomeDir returns (creating it if necessary) userID's dedicated $HOME for
// spawned child processes (run_command, checks, git - see workspace.Caps.
// HomeDir and internal/workspace/exec.go). It is a SIBLING of the user's
// cloned repos under <root>/<userID>/, never nested inside one - the fix for
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

// NodeDir returns the working directory a DAG node's tools default to - ONE
// component under the per-chat scope (<root>/<user>/<chat>/<nodeID>/) - or ""
// when nodeID can't safely name one (an un-gated call, or a planner-authored id
// with a separator: fall back to the chat root, exactly the pre-node behaviour,
// rather than fail every path the node touches).
//
// This is the fix for a live correctness bug: a plan's nodes run CONCURRENTLY in
// one chat, so with only the per-chat scope every node cloned into the same
// directory - and an explorer node told to study OpenHands sat there reading
// goose's source, because goose's clone was simply THERE. It is a DEFAULT, not a
// wall: the "/"-prefixed escape hatch still addresses the chat root, so a
// downstream node can deliberately reach an upstream node's clone.
func NodeDir(nodeID string) string {
	if !isSafePathComponent(nodeID) {
		return ""
	}
	return nodeID
}

// SetupCloneDir is the workspace-relative directory a plan's declared Setup
// PRE-step clones a repo into - the node's OWN root (NodeDir), not a
// subdirectory of it: the repo IS the node's invisible-root workspace, so
// read_file/edit_file resolve a plain relative path ("internal/foo.go") with
// no "repo/" prefix and no absolute path (a "repo" leaf here once forced a
// worker to `pwd` its way to an absolute path and shell out instead). See
// dag.Plan.Setup / internal/tools.SetupClone.
func SetupCloneDir(nodeID string) string {
	return NodeDir(nodeID)
}

// SharedRepoScope is the reserved "node" identifier a plan's repo-touching
// nodes (code-implementer/code-reviewer) resolve into when they share ONE
// declared Setup clone+branch across a depends_on chain, instead of each
// getting its own dir under SetupCloneDir(node.ID) - see dag's
// runPlanSetup/validateRepoChain. Fixed and quack-authored, never a planner-
// chosen node ID, so it can't collide with one.
const SharedRepoScope = "quack-shared-repo"

// WorktreeBranch derives the unique branch name a read-only qualifying node's
// linked git worktree is checked out on - git refuses to check the same
// branch out in two worktrees at once, and node IDs are already unique within
// a plan, so deriving from nodeID needs no separate counter or registry.
func WorktreeBranch(nodeID string) string {
	return "quack-worktree/" + nodeID
}

// EnsureDir resolves rel under the (userID, chatID) scope and creates it,
// returning the real path. Used to materialise a node's working directory at
// node entry, so the worker's very first `list_dir .` sees an (empty) dir
// instead of a "no such file" it then gropes around trying to recover from.
func (j *Jail) EnsureDir(userID, chatID, rel string) (string, error) {
	real, err := j.Resolve(userID, chatID, rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(real, 0o755); err != nil {
		return "", fmt.Errorf("workspace: create dir %q: %w", real, err)
	}
	return real, nil
}

// validateUserID is the jail-boundary guard shared by UserRoot and Resolve:
// a userID must name exactly ONE directory component directly under the
// workspace root, because Resolve joins it into the path RAW - an
// attacker-influenced identity ("../other", "a/b", an absolute path) would
// otherwise relocate the jail root itself, and the containment check would
// then verify against the WRONG root. The rule is separator/dot-traversal
// based, NOT an alphanumeric allowlist: real OIDC subjects like
// "auth0|abc123" or "user@example.com" must pass.
func validateUserID(userID string) error {
	if !isSafePathComponent(userID) {
		return ErrInvalidUserID
	}
	return nil
}

// isSafePathComponent reports whether id names exactly ONE directory component
// (no separator, no `.`/`..` traversal, already Clean) - the shared rule both
// the userID and the per-chat chatID segment obey so neither can relocate the
// scope root and defeat containment. Separator/dot based, not an alphanumeric
// allowlist (real OIDC subjects like "auth0|abc123" must pass; a chat UUID
// always passes).
func isSafePathComponent(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	if id == "." || id == ".." {
		return false
	}
	if strings.ContainsRune(id, '/') || strings.ContainsRune(id, os.PathSeparator) {
		return false
	}
	return filepath.Clean(id) == id
}

// scopeRoot is the directory a (userID, chatID) pair resolves paths under:
// <root>/<userID>/<chatID> when chatID is set, else the per-user <root>/<userID>
// (the backward-compatible fallback for a direct/un-gated call that could not
// recover a chat id). userID is always guarded (ErrInvalidUserID); a NON-empty
// chatID that isn't a safe single component is rejected (ErrInvalidChatID) so a
// crafted id can never escape the user root - an empty chatID is the only way
// to address the user root itself.
func (j *Jail) scopeRoot(userID, chatID string) (string, error) {
	userRoot, err := j.UserRoot(userID)
	if err != nil {
		return "", err
	}
	if chatID == "" {
		return userRoot, nil
	}
	if !isSafePathComponent(chatID) {
		return "", ErrInvalidChatID
	}
	return filepath.Join(userRoot, chatID), nil
}

// Resolve is the ONE path-resolution function every filesystem/git tool uses:
// it joins relPath under the (userID, chatID) scope root - <root>/<userID>/
// <chatID>/ when chatID is set, else the per-user <root>/<userID>/ - cleans it,
// resolves symlinks on the deepest existing ancestor, and verifies the result
// is prefix-contained in that scope root. Absolute relPath, `..` escapes, and
// symlinks pointing outside the scope all fail identically with ErrEscape - "no
// exceptions" per the isolation design. A symlink that stays inside resolves and
// works normally. userID is guarded first (ErrInvalidUserID) and a non-empty
// chatID second (ErrInvalidChatID): both must be single path components, or the
// scope root itself would relocate. chatID "" resolves the per-user root (the
// backward-compatible fallback for a direct/un-gated call - see the per-chat
// scoping in internal/tools).
func (j *Jail) Resolve(userID, chatID, relPath string) (string, error) {
	scopeRoot, err := j.scopeRoot(userID, chatID)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relPath) {
		return "", ErrEscape
	}
	joined := filepath.Join(scopeRoot, relPath)
	if !withinRoot(scopeRoot, joined) {
		return "", ErrEscape
	}

	realScopeRoot, err := resolveDeepestExisting(scopeRoot)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve scope root: %w", err)
	}
	real, err := resolveDeepestExisting(joined)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve path: %w", err)
	}
	if !withinRoot(realScopeRoot, real) {
		return "", ErrEscape
	}
	return real, nil
}

// RemoveChatScope deletes a chat's per-chat workspace subtree
// (<root>/<userID>/<chatID>/) and everything under it - the lifecycle
// counterpart of per-chat scoping, called when a chat is deleted so its working
// tree doesn't leak forever. userID and chatID are validated as single path
// components (scopeRoot, RESOLVE-identical), so a crafted id can never make the
// removal escape the user root; an EMPTY chatID is rejected (ErrInvalidChatID)
// so this can never remove the whole user root. A non-existent dir is a clean
// no-op (nil). Callers treat any error as best-effort (log + continue): cleanup
// must never block or fail the chat delete itself.
func (j *Jail) RemoveChatScope(userID, chatID string) error {
	if strings.TrimSpace(chatID) == "" {
		return ErrInvalidChatID
	}
	root, err := j.scopeRoot(userID, chatID)
	if err != nil {
		return err
	}
	// Defense in depth: never remove the user root or the jail root, even if
	// scopeRoot somehow produced one.
	userRoot, err := j.UserRoot(userID)
	if err != nil {
		return err
	}
	if root == userRoot || root == j.root {
		return ErrInvalidChatID
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("workspace: remove chat scope %q: %w", root, err)
	}
	return nil
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
			// Reached the filesystem root and nothing exists on the path at all -
			// there are no symlinks to resolve; the cleaned path is already real.
			return p, nil
		}
		trailing = append(trailing, filepath.Base(cur))
		cur = parent
	}
}
