package workspace

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// baselineTempPrefix mirrors vetting's os.MkdirTemp("", "quack-base-") prefix.
const baselineTempPrefix = "quack-base-"

// GCConfig is the periodic reaper's tunables. TTL-based for chats/scratch;
// HomeMaxBytes is quota-based (the agent home has no per-entry idle time to
// expire - it is one shared directory for the whole user).
type GCConfig struct {
	Enabled bool
	// ChatTTL/ScratchTTL/HomeMaxBytes <= 0 skip that sweep class entirely.
	ChatTTL      time.Duration
	ScratchTTL   time.Duration
	HomeMaxBytes int64
	Interval     time.Duration
	// BaselineTempDir overrides where sweepBaselineTemp globs for quack-base-*
	// dirs; empty means the real os.TempDir(). Tests set this so a concurrent
	// package sharing the machine-wide temp dir can't add or remove entries
	// this sweep counts.
	BaselineTempDir string
}

// ActiveChatFunc reports whether the chat behind an on-disk directory name
// (raw chat id or its ChatDirName rewrite) has a run in flight. nil skips every chat.
type ActiveChatFunc func(chatDir string) bool

// WorktreePruner detaches a linked worktree before GC removes it. nil = remove-only.
type WorktreePruner func(ctx context.Context, dir string) error

// GCResult summarizes one sweep.
type GCResult struct {
	ChatsRemoved   int
	ScratchRemoved int
	HomeReset      int
	BytesReclaimed int64
}

func (r GCResult) empty() bool {
	return r.ChatsRemoved == 0 && r.ScratchRemoved == 0 && r.HomeReset == 0
}

// RunGC sweeps once immediately, then on cfg.Interval, until ctx is cancelled.
func RunGC(ctx context.Context, jail *Jail, cfg GCConfig, isActive ActiveChatFunc, prune WorktreePruner) {
	if !cfg.Enabled {
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	sweepAndLog(ctx, jail, cfg, isActive, prune)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepAndLog(ctx, jail, cfg, isActive, prune)
		}
	}
}

func sweepAndLog(ctx context.Context, jail *Jail, cfg GCConfig, isActive ActiveChatFunc, prune WorktreePruner) {
	res := Sweep(ctx, jail, cfg, isActive, prune)
	if res.empty() {
		slog.Debug("workspace gc: nothing to reclaim", "component", "workspace")
		return
	}
	slog.Info("workspace gc: swept", "component", "workspace",
		"chats_removed", res.ChatsRemoved, "scratch_removed", res.ScratchRemoved,
		"home_reset", res.HomeReset, "bytes_reclaimed", res.BytesReclaimed)
}

// Sweep runs one GC pass: idle chat scopes, then scratch (baseline worktrees
// + .quack-home/tmp), then the agent home quota.
func Sweep(ctx context.Context, jail *Jail, cfg GCConfig, isActive ActiveChatFunc, prune WorktreePruner) GCResult {
	var res GCResult
	if cfg.ChatTTL > 0 {
		n, b := sweepChatScopes(ctx, jail, cfg.ChatTTL, isActive, prune)
		res.ChatsRemoved += n
		res.BytesReclaimed += b
	}
	if cfg.ScratchTTL > 0 {
		n, b := sweepBaselineTemp(ctx, cfg.ScratchTTL, cfg.BaselineTempDir, prune)
		res.ScratchRemoved += n
		res.BytesReclaimed += b
		n, b = sweepHomeTmp(cfg.ScratchTTL, jail)
		res.ScratchRemoved += n
		res.BytesReclaimed += b
	}
	if cfg.HomeMaxBytes > 0 {
		n, b := sweepAgentHome(ctx, jail, cfg.HomeMaxBytes, isActive)
		res.HomeReset += n
		res.BytesReclaimed += b
	}
	return res
}

// sweepChatScopes removes chat scopes whose mtime predates ttl, skipping active chats.
func sweepChatScopes(ctx context.Context, jail *Jail, ttl time.Duration, isActive ActiveChatFunc, prune WorktreePruner) (removed int, bytes int64) {
	userEntries, err := os.ReadDir(jail.Root())
	if err != nil {
		slog.Warn("workspace gc: list jail root failed", "component", "workspace", "err", err)
		return 0, 0
	}
	cutoff := time.Now().Add(-ttl)
	for _, ue := range userEntries {
		if ctx.Err() != nil {
			return removed, bytes
		}
		if !ue.IsDir() {
			continue
		}
		userID := ue.Name()
		userRoot, err := jail.UserRoot(userID)
		if err != nil {
			continue // not a valid single-component userID; not ours to touch
		}
		chatEntries, err := os.ReadDir(userRoot)
		if err != nil {
			continue
		}
		for _, ce := range chatEntries {
			chatDir := ce.Name()
			if !ce.IsDir() || chatDir == homeDirName {
				continue
			}
			if isActive == nil || isActive(chatDir) {
				continue
			}
			scope := filepath.Join(userRoot, chatDir)
			fi, err := os.Stat(scope)
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			sz := dirSize(scope)
			pruneWorktreesUnder(ctx, scope, prune)
			if err := jail.RemoveChatScope(userID, chatDir); err != nil {
				slog.Warn("workspace gc: remove chat scope failed", "component", "workspace",
					"user", userID, "chat", chatDir, "err", err)
				continue
			}
			removed++
			bytes += sz
		}
	}
	return removed, bytes
}

// sweepBaselineTemp removes orphaned baseline-check worktrees (quack-base-*) whose mtime predates ttl.
func sweepBaselineTemp(ctx context.Context, ttl time.Duration, tempDir string, prune WorktreePruner) (removed int, bytes int64) {
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(tempDir, baselineTempPrefix+"*"))
	if err != nil {
		slog.Warn("workspace gc: glob baseline scratch failed", "component", "workspace", "err", err)
		return 0, 0
	}
	cutoff := time.Now().Add(-ttl)
	for _, dir := range matches {
		if ctx.Err() != nil {
			return removed, bytes
		}
		fi, err := os.Stat(dir)
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		sz := dirSize(dir)
		pruneWorktreesUnder(ctx, dir, prune)
		if err := RemoveAllForce(dir); err != nil {
			slog.Warn("workspace gc: remove baseline scratch failed", "component", "workspace", "dir", dir, "err", err)
			continue
		}
		removed++
		bytes += sz
	}
	return removed, bytes
}

// sweepHomeTmp removes stale .quack-home/tmp entries. Caches (npm/go/gradle) are left alone.
func sweepHomeTmp(ttl time.Duration, jail *Jail) (removed int, bytes int64) {
	userEntries, err := os.ReadDir(jail.Root())
	if err != nil {
		return 0, 0
	}
	cutoff := time.Now().Add(-ttl)
	for _, ue := range userEntries {
		if !ue.IsDir() {
			continue
		}
		homeDir, err := jail.HomeDir(ue.Name())
		if err != nil {
			continue
		}
		tmpDir := filepath.Join(homeDir, "tmp")
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(tmpDir, e.Name())
			fi, err := os.Stat(p)
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			sz := dirSize(p)
			if err := RemoveAllForce(p); err != nil {
				slog.Warn("workspace gc: remove home tmp entry failed", "component", "workspace", "path", p, "err", err)
				continue
			}
			removed++
			bytes += sz
		}
	}
	return removed, bytes
}

// sweepAgentHome resets a user's ACP agent home (opencode.db, snapshot,
// tool-output, log - opencode's private state, never quack's own) whole once
// it exceeds maxBytes. The home is shared across every one of the user's
// chats, so it has no TTL of its own; instead this only fires when
// anyChatActiveForUser proves none of them has a round in flight, the same
// isActive signal sweepChatScopes trusts to protect a live chat's clone.
func sweepAgentHome(ctx context.Context, jail *Jail, maxBytes int64, isActive ActiveChatFunc) (reset int, bytes int64) {
	userEntries, err := os.ReadDir(jail.Root())
	if err != nil {
		slog.Warn("workspace gc: list jail root failed", "component", "workspace", "err", err)
		return 0, 0
	}
	for _, ue := range userEntries {
		if ctx.Err() != nil {
			return reset, bytes
		}
		if !ue.IsDir() {
			continue
		}
		userID := ue.Name()
		if anyChatActiveForUser(jail, userID, isActive) {
			continue
		}
		home, err := jail.HomeDir(userID)
		if err != nil {
			continue
		}
		sz := dirSize(home)
		if sz < maxBytes {
			continue
		}
		if err := resetHomeDir(home); err != nil {
			slog.Warn("workspace gc: reset agent home failed", "component", "workspace", "user", userID, "err", err)
			continue
		}
		reset++
		bytes += sz
	}
	return reset, bytes
}

// anyChatActiveForUser reports whether any of userID's known chats has a run
// in flight. A nil isActive fails closed (treated as active - can't prove
// otherwise), matching sweepChatScopes.
func anyChatActiveForUser(jail *Jail, userID string, isActive ActiveChatFunc) bool {
	if isActive == nil {
		return true
	}
	userRoot, err := jail.UserRoot(userID)
	if err != nil {
		return true
	}
	entries, err := os.ReadDir(userRoot)
	if err != nil {
		return true // can't enumerate chats, so can't prove none is live
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == homeDirName {
			continue
		}
		if isActive(e.Name()) {
			return true
		}
	}
	return false
}

// resetHomeDir empties home in one shot - opencode.db's schema is not ours,
// so we reclaim the whole opaque directory rather than edit inside it.
func resetHomeDir(home string) error {
	if err := RemoveAllForce(home); err != nil {
		return err
	}
	return os.MkdirAll(home, 0o700)
}

// pruneWorktreesUnder detaches linked worktrees before root removal. No-op when prune is nil.
func pruneWorktreesUnder(ctx context.Context, root string, prune WorktreePruner) {
	if prune == nil {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != ".git" {
			return nil
		}
		wt := filepath.Dir(path)
		if perr := prune(ctx, wt); perr != nil {
			slog.Debug("workspace gc: worktree prune failed; removing anyway", "component", "workspace", "dir", wt, "err", perr)
		}
		return nil
	})
}

// RemoveAllForce deletes path, restoring write permission on directories
// that deny it before retrying once. Go's module cache marks cached deps
// 0444 and their parent dirs 0555, so a plain RemoveAll fails with EACCES on
// unlink - the same reason `go clean -modcache` has to exist. Fast path
// first: the chmod walk only runs once a plain removal has actually failed.
// WalkDir never follows symlinks (a symlink's own DirEntry.IsDir() is
// false), so a symlink inside path is skipped rather than chmod'd through to
// whatever it points at outside the tree.
func RemoveAllForce(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// dirSize sums regular-file sizes under root. Best-effort.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
