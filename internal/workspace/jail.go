// Package workspace: the isolation boundary every filesystem/git tool resolves paths through. Stdlib only.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Uniform rejection for any path outside the caller's jail. One error by design: one thing for the model to learn.
var ErrEscape = errors.New("path escapes your workspace")

// Rejects a userID that can't safely name one directory component. Distinct from ErrEscape: operator fix, not model learning.
var ErrInvalidUserID = errors.New("workspace: invalid user id")

// Rejects a chatID that can't safely name one directory component. Empty chatID means "no per-chat scope" (backward compatible).
var ErrInvalidChatID = errors.New("workspace: invalid chat id")

// Rejects a nodeID that can't safely name one directory component (ScratchDir).
var ErrInvalidNodeID = errors.New("workspace: invalid node id")

// One configured workspace root; per-user boundaries derived at resolve time.
type Jail struct {
	// Absolute, symlink-resolved workspace root.
	root string
}

// Builds a Jail rooted at root (created if missing), canonicalized so containment checks compare real paths.
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

// Canonical jail root for logging/diagnostics.
func (j *Jail) Root() string { return j.root }

// Jail directory for userID (unresolved). May not exist; callers create it themselves.
func (j *Jail) UserRoot(userID string) (string, error) {
	if err := validateUserID(userID); err != nil {
		return "", err
	}
	return filepath.Join(j.root, userID), nil
}

// Dot-prefixed so it reads as infrastructure, not a cloned repo.
const homeDirName = ".quack-home"

// Dedicated $HOME for child processes. Created 0o700, outside cloned repos so caches aren't swept up by git_commit.
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

// ScratchDir is a private, per-node writable tmp dir for a sandboxed worker's
// own scratch use (mktemp, heredocs, a build's tmp files) - a home for the
// TMPDIR grant that doesn't collide with, or get swept alongside, another
// node's. Nested under HomeDir (never inside the node's own workspace: a
// read-only node's tree must stay wholly immutable), one directory component
// per node so workspace gc's existing per-entry sweepHomeTmp TTL sweep (see
// gc.go) reaps it with no changes of its own.
func (j *Jail) ScratchDir(userID, chatID, nodeID string) (string, error) {
	if !isSafePathComponent(chatID) {
		return "", ErrInvalidChatID
	}
	if !isSafePathComponent(nodeID) {
		return "", ErrInvalidNodeID
	}
	home, err := j.HomeDir(userID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "tmp", ChatDirName(chatID)+"__"+nodeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("workspace: create scratch dir %q: %w", dir, err)
	}
	return dir, nil
}

// Working directory a DAG node's tools default to (one component under chat scope). "" falls back to chat root.
func NodeDir(nodeID string) string {
	if !isSafePathComponent(nodeID) {
		return ""
	}
	return nodeID
}

// Workspace-relative directory a Setup pre-step clones into. The repo IS the node's workspace (no "repo/" prefix needed).
func SetupCloneDir(nodeID string) string {
	return NodeDir(nodeID)
}

// Reserved node ID for nodes sharing one clone across a depends_on chain. Fixed, never a planner-chosen ID.
const SharedRepoScope = "quack-shared-repo"

// Unique branch name for a qualifying node's linked worktree. Derived from nodeID (no registry needed).
func WorktreeBranch(nodeID string) string {
	return "quack-worktree/" + nodeID
}

// Resolves rel under (userID, chatID) scope and creates it. So the worker's first list_dir sees an (empty) dir.
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

// Jail-boundary guard: userID must name exactly one directory component. Separator/dot based, not alphanumeric (OIDC subjects like "auth0|abc123" must pass).
func validateUserID(userID string) error {
	if !isSafePathComponent(userID) {
		return ErrInvalidUserID
	}
	return nil
}

// Reports whether id names exactly one directory component (no separator, no `.`/`..` traversal).
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

// Path/shell-hostile runes for a directory name (':' breaks node module resolution and PATH-style parsing).
var hostileRunes = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// ChatDirName maps a chat id to its on-disk directory component. Hostile runes
// become '-', with a short hash of the raw id appended so rewritten ids can't
// collide (ext:a:b vs ext-a-b). Clean ids map to themselves. The chat id itself
// (DB/API/UI) never changes - only the directory name.
func ChatDirName(chatID string) string {
	clean := hostileRunes.ReplaceAllString(chatID, "-")
	if clean == chatID {
		return chatID
	}
	sum := sha256.Sum256([]byte(chatID))
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// Directory a (userID, chatID) pair resolves paths under. chatID="" falls back to per-user root.
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
	dir := ChatDirName(chatID)
	if dir != chatID {
		// Zero-migration: chats from before sanitization keep their raw-named dir.
		if fi, err := os.Stat(filepath.Join(userRoot, chatID)); err == nil && fi.IsDir() {
			dir = chatID
		}
	}
	return filepath.Join(userRoot, dir), nil
}

// Path-resolution for every filesystem/git tool: joins relPath under scope root, resolves symlinks, verifies containment.
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

// Deletes a chat's workspace subtree. Empty chatID rejected (ErrInvalidChatID) so it can never delete the user root.
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
	if err := removeAll(root); err != nil {
		return fmt.Errorf("workspace: remove chat scope %q: %w", root, err)
	}
	return nil
}

// Reports whether path is root or a descendant. Both must be Clean'd absolute paths.
func withinRoot(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Resolves symlinks on the deepest existing ancestor of p, rejoining trailing nonexistent components.
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
