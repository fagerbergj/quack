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

// GCConfig: TTLs for chats and scratch; the agent home is one shared dir per user, so it gets a quota.
type GCConfig struct {
	Enabled bool
	// ChatTTL/ScratchTTL/HomeMaxBytes <= 0 skip that sweep class entirely.
	ChatTTL      time.Duration
	ScratchTTL   time.Duration
	HomeMaxBytes int64
	Interval     time.Duration
	// BaselineTempDir overrides os.TempDir() so tests aren't disturbed by other packages' temp entries.
	BaselineTempDir string
}

// ActiveChatFunc takes a raw chat id or its ChatDirName rewrite. nil skips every chat.
type ActiveChatFunc func(chatDir string) bool

// WorktreePruner detaches a linked worktree before GC removes it; root bounds its owning clone. nil = remove-only.
type WorktreePruner func(ctx context.Context, root, dir string) error

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

// Sweep: idle chat scopes, then scratch (baseline worktrees, .quack-home/tmp), then the agent home quota.
func Sweep(ctx context.Context, jail *Jail, cfg GCConfig, isActive ActiveChatFunc, prune WorktreePruner) GCResult {
	var res GCResult
	if cfg.ChatTTL > 0 {
		n, b := sweepChatScopes(ctx, jail, cfg.ChatTTL, isActive, prune)
		res.ChatsRemoved += n
		res.BytesReclaimed += b
	}
	if cfg.ScratchTTL > 0 {
		n, b := sweepBaselineTemp(ctx, jail.Root(), cfg.ScratchTTL, cfg.BaselineTempDir, prune)
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
			pruneWorktreesUnder(ctx, scope, scope, prune)
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
func sweepBaselineTemp(ctx context.Context, jailRoot string, ttl time.Duration, tempDir string, prune WorktreePruner) (removed int, bytes int64) {
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
		pruneWorktreesUnder(ctx, jailRoot, dir, prune)
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

// sweepAgentHome resets a user's ACP agent home once it exceeds maxBytes, only when none of their chats is live.
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

// anyChatActiveForUser fails closed: a nil isActive counts as active.
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

// resetHomeDir: the ACP agent's on-disk state isn't a schema we own, so it is reclaimed whole.
func resetHomeDir(home string) error {
	if err := RemoveAllForce(home); err != nil {
		return err
	}
	return os.MkdirAll(home, 0o700)
}

// pruneWorktreesUnder detaches linked worktrees under dir; each owning clone is bounded by cloneRoot.
func pruneWorktreesUnder(ctx context.Context, cloneRoot, dir string, prune WorktreePruner) {
	if prune == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != ".git" {
			return nil
		}
		wt := filepath.Dir(path)
		if perr := prune(ctx, cloneRoot, wt); perr != nil {
			slog.Debug("workspace gc: worktree prune failed; removing anyway", "component", "workspace", "dir", wt, "err", perr)
		}
		return nil
	})
}

// RemoveAllForce retries once after restoring dir write bits: Go's module cache is read-only, so RemoveAll
// hits EACCES. WalkDir never follows symlinks, so nothing outside the tree is chmod'd.
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
