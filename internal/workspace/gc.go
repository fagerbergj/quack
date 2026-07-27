package workspace

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// baselineTempPrefix mirrors internal/vetting/baseline.go's
// os.MkdirTemp("", "quack-base-") prefix - the only name pattern this sweep
// touches under the real OS temp dir, which lies outside the jail and so
// gets its own containment story (a fixed glob, nothing broader) instead of
// jail.Resolve.
const baselineTempPrefix = "quack-base-"

// GCConfig is the periodic reaper's tunables (workspace.gc in quack.yaml -
// see internal/config for defaulting/validation). TTL-based, not
// quota-based: filling the volume inside the TTL window isn't covered here -
// a high-water-mark pass (evict oldest until usage drops below a threshold)
// is the upgrade path if that ever bites.
type GCConfig struct {
	Enabled bool
	// ChatTTL/ScratchTTL <= 0 skip that sweep class entirely.
	ChatTTL    time.Duration
	ScratchTTL time.Duration
	Interval   time.Duration
}

// ActiveChatFunc reports whether chatID currently has a run registered
// (queued or executing) - RunGC's hard stop against reaping a live run's
// clone mid-round. See stream.Hub.HasRegisteredRun. nil is treated as "can't
// prove anything is inactive": every chat scope is skipped, never reaped.
type ActiveChatFunc func(chatID string) bool

// WorktreePruner detaches a linked git worktree from its parent clone's
// bookkeeping before GC removes it - see internal/tools.PruneWorktree, the
// one implementation (git operations don't belong in this dependency-free
// package). nil degrades to plain removal only: worst case the parent's next
// `git worktree add`/`prune` just has stale bookkeeping to clear itself.
type WorktreePruner func(ctx context.Context, dir string) error

// GCResult summarizes one sweep - RunGC logs it, or stays quiet at Debug when
// nothing moved so the log isn't noise every interval.
type GCResult struct {
	ChatsRemoved   int
	ScratchRemoved int
	BytesReclaimed int64
}

func (r GCResult) empty() bool { return r.ChatsRemoved == 0 && r.ScratchRemoved == 0 }

// RunGC sweeps once immediately, then on cfg.Interval, until ctx is
// cancelled. Call it as `go RunGC(...)` - it never returns early on its own,
// and startup must not block on a sweep of a volume that may hold years of
// clones.
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
		"bytes_reclaimed", res.BytesReclaimed)
}

// Sweep runs one GC pass: chat scopes idle longer than cfg.ChatTTL (skipping
// any chat isActive reports live), then scratch - the vetting gate's
// baseline worktrees under the OS temp dir, plus .quack-home/tmp entries -
// idle longer than cfg.ScratchTTL. Exported so tests (and RunGC) can drive
// one pass directly, without a ticker.
func Sweep(ctx context.Context, jail *Jail, cfg GCConfig, isActive ActiveChatFunc, prune WorktreePruner) GCResult {
	var res GCResult
	if cfg.ChatTTL > 0 {
		n, b := sweepChatScopes(ctx, jail, cfg.ChatTTL, isActive, prune)
		res.ChatsRemoved += n
		res.BytesReclaimed += b
	}
	if cfg.ScratchTTL > 0 {
		n, b := sweepBaselineTemp(ctx, cfg.ScratchTTL, prune)
		res.ScratchRemoved += n
		res.BytesReclaimed += b
		n, b = sweepHomeTmp(cfg.ScratchTTL, jail)
		res.ScratchRemoved += n
		res.BytesReclaimed += b
	}
	return res
}

// sweepChatScopes removes <root>/<userID>/<chatID>/ scopes whose own mtime
// predates ttl, skipping any chat isActive reports as having a run in flight
// - the one unacceptable failure here is deleting a live run's clone
// mid-round. userIDs and chatIDs are read straight off disk (jail.Root()'s
// own listing), never re-derived, and removal goes through
// jail.RemoveChatScope - the same containment guard the chat-delete
// lifecycle path already relies on.
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
			chatID := ce.Name()
			if !ce.IsDir() || chatID == homeDirName {
				continue
			}
			if isActive == nil || isActive(chatID) {
				continue
			}
			scope := filepath.Join(userRoot, chatID)
			fi, err := os.Stat(scope)
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			sz := dirSize(scope)
			pruneWorktreesUnder(ctx, scope, prune)
			if err := jail.RemoveChatScope(userID, chatID); err != nil {
				slog.Warn("workspace gc: remove chat scope failed", "component", "workspace",
					"user", userID, "chat", chatID, "err", err)
				continue
			}
			removed++
			bytes += sz
		}
	}
	return removed, bytes
}

// sweepBaselineTemp removes orphaned baseline-check worktrees
// (internal/vetting/baseline.go's os.MkdirTemp("", "quack-base-")) whose
// mtime predates ttl. These are single-round scratch that baseline.go always
// cleans up itself; only a crash mid-check leaves one behind.
func sweepBaselineTemp(ctx context.Context, ttl time.Duration, prune WorktreePruner) (removed int, bytes int64) {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), baselineTempPrefix+"*"))
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
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("workspace gc: remove baseline scratch failed", "component", "workspace", "dir", dir, "err", err)
			continue
		}
		removed++
		bytes += sz
	}
	return removed, bytes
}

// sweepHomeTmp removes stale entries under each user's private scratch dir
// (<root>/<userID>/.quack-home/tmp/ - see homeTmpDir in sandbox.go) whose
// mtime predates ttl. NOT the caches alongside it (npm/go/gradle) - those
// are left alone by design (a cache TTL would make every subsequent run
// re-download its world).
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
			if err := os.RemoveAll(p); err != nil {
				slog.Warn("workspace gc: remove home tmp entry failed", "component", "workspace", "path", p, "err", err)
				continue
			}
			removed++
			bytes += sz
		}
	}
	return removed, bytes
}

// pruneWorktreesUnder walks root for linked-worktree pointer files (a
// regular file literally named ".git" - a plain clone's own ".git" is a
// directory and never matches) and detaches each one from its parent's
// bookkeeping via prune BEFORE root is removed wholesale - see
// WorktreePruner's doc. No-op when prune is nil; best-effort otherwise, since
// the removal that follows must proceed either way.
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

// dirSize sums regular-file sizes under root, for the sweep's
// bytes-reclaimed log line - best-effort, an unreadable entry just doesn't
// count.
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
