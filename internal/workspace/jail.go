// Package workspace is the isolation boundary every filesystem/git tool resolves paths through.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrEscape is deliberately one error for every outside path: one thing for the model to learn.
var ErrEscape = errors.New("path escapes your workspace")

// ErrInvalidUserID is distinct from ErrEscape: it needs an operator fix, not model learning.
var ErrInvalidUserID = errors.New("workspace: invalid user id")

// Empty chatID means no per-chat scope.
var ErrInvalidChatID = errors.New("workspace: invalid chat id")

var ErrInvalidNodeID = errors.New("workspace: invalid node id")

// Jail derives per-user boundaries from one root at resolve time.
type Jail struct {
	// Absolute and symlink-resolved.
	root string
}

// NewJail canonicalizes root so containment checks compare real paths.
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

func (j *Jail) Root() string { return j.root }

// UserRoot is unresolved and may not exist; callers create it.
func (j *Jail) UserRoot(userID string) (string, error) {
	if err := validateUserID(userID); err != nil {
		return "", err
	}
	return filepath.Join(j.root, userID), nil
}

// Dot-prefixed so it reads as infrastructure, not a cloned repo.
const homeDirName = ".quack-home"

// HomeDir is outside cloned repos so tool caches aren't swept up by git_commit.
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

func (j *Jail) nodeHomeDir(userID, chatID, nodeID, subdir, label string) (string, error) {
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
	dir := filepath.Join(home, subdir, ChatDirName(chatID)+"__"+nodeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("workspace: create %s %q: %w", label, dir, err)
	}
	return dir, nil
}

// ScratchDir is a per-node TMPDIR under HomeDir, never in the node's tree (read-only trees stay immutable);
// one component per node so sweepHomeTmp's TTL reaps it.
func (j *Jail) ScratchDir(userID, chatID, nodeID string) (string, error) {
	return j.nodeHomeDir(userID, chatID, nodeID, "tmp", "scratch dir")
}

// ACPStateDir holds the ACP shim's session persistence; unlike ScratchDir, never named in the prompt.
func (j *Jail) ACPStateDir(userID, chatID, nodeID string) (string, error) {
	return j.nodeHomeDir(userID, chatID, nodeID, "acp-state", "acp state dir")
}

// NodeDir: "" falls back to the chat root.
func NodeDir(nodeID string) string {
	if !isSafePathComponent(nodeID) {
		return ""
	}
	return nodeID
}

// The cloned repo is the node's workspace itself, with no "repo/" prefix.
func SetupCloneDir(nodeID string) string {
	return NodeDir(nodeID)
}

// Reserved node ID for nodes sharing one clone across a depends_on chain; never planner-chosen.
const SharedRepoScope = "quack-shared-repo"

func WorktreeBranch(nodeID string) string {
	return "quack-worktree/" + nodeID
}

// EnsureDir creates rel so the worker's first list_dir sees an empty dir rather than an error.
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

// Separator/dot based, not alphanumeric: OIDC subjects like "auth0|abc123" must pass.
func validateUserID(userID string) error {
	if !isSafePathComponent(userID) {
		return ErrInvalidUserID
	}
	return nil
}

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

// ':' breaks node module resolution and PATH-style parsing.
var hostileRunes = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// ChatDirName replaces hostile runes with '-' and appends a hash of the raw id so rewrites
// can't collide (ext:a:b vs ext-a-b). Clean ids map to themselves.
func ChatDirName(chatID string) string {
	clean := hostileRunes.ReplaceAllString(chatID, "-")
	if clean == chatID {
		return chatID
	}
	sum := sha256.Sum256([]byte(chatID))
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// chatID="" falls back to the per-user root.
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
		// Existing raw-named dirs keep working without a migration.
		if fi, err := os.Stat(filepath.Join(userRoot, chatID)); err == nil && fi.IsDir() {
			dir = chatID
		}
	}
	return filepath.Join(userRoot, dir), nil
}

// Resolve joins relPath under the scope root, resolves symlinks and verifies containment.
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

// ResolveRepoDir is Resolve for a dir quack clones or adds a worktree into, but never follows a symlink: one standing
// in for the dir or a parent below the scope root is removed with a WARN, so the caller re-clones into a real dir.
func (j *Jail) ResolveRepoDir(userID, chatID, relPath string) (string, error) {
	scopeRoot, err := j.scopeRoot(userID, chatID)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(relPath) {
		return "", ErrEscape
	}
	p := scopeRoot
	for _, c := range strings.Split(filepath.Clean(relPath), string(filepath.Separator)) {
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		slog.Warn("git: a symlink stands in for a repository dir; removing it to re-clone into a real dir",
			"component", "workspace", "link", p)
		if err := os.Remove(p); err != nil {
			return "", fmt.Errorf("workspace: remove symlink %s: %w", p, err)
		}
		break
	}
	return filepath.Join(scopeRoot, relPath), nil
}

// RemoveChatScope rejects an empty chatID so it can never delete the user root.
func (j *Jail) RemoveChatScope(userID, chatID string) error {
	if strings.TrimSpace(chatID) == "" {
		return ErrInvalidChatID
	}
	root, err := j.scopeRoot(userID, chatID)
	if err != nil {
		return err
	}
	// Defense in depth: never remove the user or jail root.
	userRoot, err := j.UserRoot(userID)
	if err != nil {
		return err
	}
	if root == userRoot || root == j.root {
		return ErrInvalidChatID
	}
	if err := RemoveAllForce(root); err != nil {
		return fmt.Errorf("workspace: remove chat scope %q: %w", root, err)
	}
	return nil
}

// RemoveACPState: ACP state lives under HomeDir so a session survives a clone re-provision,
// so RemoveChatScope doesn't cover it. Empty chatID rejected.
func (j *Jail) RemoveACPState(userID, chatID string) error {
	if strings.TrimSpace(chatID) == "" {
		return ErrInvalidChatID
	}
	home, err := j.HomeDir(userID)
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(home, "acp-state", ChatDirName(chatID)+"__*"))
	if err != nil {
		return fmt.Errorf("workspace: glob acp state for chat %q: %w", chatID, err)
	}
	for _, dir := range matches {
		if err := RemoveAllForce(dir); err != nil {
			return fmt.Errorf("workspace: remove acp state %q: %w", dir, err)
		}
	}
	return nil
}

// Both must be Clean'd absolute paths.
func withinRoot(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolveDeepestExisting resolves the deepest existing ancestor of p and rejoins the nonexistent rest.
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
			// Nothing on the path exists, so there are no symlinks to resolve.
			return p, nil
		}
		trailing = append(trailing, filepath.Base(cur))
		cur = parent
	}
}
