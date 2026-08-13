package memory

import (
	"context"
	"maps"
	"slices"
	"sort"
	"time"
)

// clusterWindow is the temporal-proximity threshold the burst-dedupe cluster
// (design doc §4(c)) chains on: consecutive unverified memories from the same
// chat_id, minted no more than this apart, join one cluster.
const clusterWindow = 15 * time.Minute

// consolidatorAuthor tags a point the sweep itself writes (an UPDATE merging
// a burst's wording), distinct from the agent name a live commit stamps.
const consolidatorAuthor = "memory-consolidator"

// RunConsolidationSweep runs the periodic burst-dedupe pass (design doc
// §4(c)) plus retention (design doc §6) - once immediately, then every
// interval, until ctx is done. Mirrors ledger.RunRetentionSweep's shape and
// disable convention: interval <= 0 is a no-op, no goroutine or ticker at
// all. retentionDays <= 0 disables only the hard-delete half; the
// consolidation half still runs on interval.
func (s *Store) RunConsolidationSweep(ctx context.Context, interval time.Duration, retentionDays int) {
	if interval <= 0 {
		return
	}
	s.sweepOnce(ctx, retentionDays)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx, retentionDays)
		}
	}
}

func (s *Store) sweepOnce(ctx context.Context, retentionDays int) {
	s.consolidateOnce(ctx)
	s.retentionOnce(ctx, retentionDays)
}

// consolidateOnce clusters currently-valid unverified memories by bucket +
// provenance chat_id + temporal proximity and asks the consolidation model to
// dedupe each cluster (design doc §4(c)). Off the hot path: reached only from
// the ticker, never inlined in a commit.
func (s *Store) consolidateOnce(ctx context.Context) {
	pts, err := s.idx.list(ctx, nil, 0, 0, false) // every bucket, currently-valid only
	if err != nil {
		s.log.Warn("consolidation sweep: list failed", "err", err)
		return
	}
	byBucket := map[string][]scored{}
	for _, p := range pts {
		if p.Status == string(StatusReinforced) {
			continue // earned trust; never a dedupe candidate
		}
		byBucket[p.Scope] = append(byBucket[p.Scope], p)
	}
	clusters, applied := 0, 0
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) { // deterministic order
		for _, cluster := range burstClusters(byBucket[bucket]) {
			clusters++
			n, err := s.consolidateCluster(ctx, bucket, cluster)
			if err != nil {
				s.log.Warn("consolidation sweep: cluster failed", "bucket", bucket, "err", err)
				continue
			}
			applied += n
		}
	}
	s.log.Info("consolidation sweep", "clusters", clusters, "ops_applied", applied)
}

// burstClusters groups pts (already one bucket, already filtered to
// unverified) into bursts: same ChatID, chained by MintedAt gaps no larger
// than clusterWindow. Only clusters of >=2 are returned - a lone memory has
// nothing to dedupe against. A point with no ChatID or an unparsable
// MintedAt can't be placed in time, so it's dropped from clustering rather
// than guessed into one.
func burstClusters(pts []scored) [][]scored {
	byChat := map[string][]scored{}
	for _, p := range pts {
		if p.ChatID == "" {
			continue
		}
		byChat[p.ChatID] = append(byChat[p.ChatID], p)
	}
	var out [][]scored
	for _, chatID := range slices.Sorted(maps.Keys(byChat)) { // deterministic order
		group := byChat[chatID]
		sort.Slice(group, func(i, j int) bool { return group[i].MintedAt < group[j].MintedAt })
		var cur []scored
		var last time.Time
		for _, p := range group {
			t, err := time.Parse(time.RFC3339, p.MintedAt)
			if err != nil {
				if len(cur) >= 2 {
					out = append(out, cur)
				}
				cur = nil
				continue
			}
			if len(cur) > 0 && t.Sub(last) > clusterWindow {
				if len(cur) >= 2 {
					out = append(out, cur)
				}
				cur = nil
			}
			cur = append(cur, p)
			last = t
		}
		if len(cur) >= 2 {
			out = append(out, cur)
		}
	}
	return out
}

// consolidateCluster runs the dedupe prompt variant over one burst and
// applies its ops - the sweep's counterpart to commitTo, using the cluster
// itself as both the candidate set and the neighbours the model may
// reference (so "duplicate of <id>" always names a real, shown id).
func (s *Store) consolidateCluster(ctx context.Context, bucket string, cluster []scored) (int, error) {
	neighbours := make([]neighbour, len(cluster))
	valid := make(map[string]neighbour, len(cluster))
	for i, p := range cluster {
		n := neighbour{
			ID: p.ID, Content: p.Content, ChatID: p.ChatID, NodeID: p.NodeID, Source: p.Source,
			MintedAt: p.MintedAt, Status: p.Status, ValidFrom: p.ValidFrom, ReinforcementCount: p.ReinforcementCount,
		}
		neighbours[i] = n
		valid[p.ID] = n
	}
	ops, err := s.decideDedupe(ctx, neighbours)
	if err != nil {
		return 0, err
	}
	if len(ops) == 0 {
		return 0, nil
	}
	// Provenance for a fallback fresh ADD (the dedupe prompt asks for
	// UPDATE/DELETE/NOOP; ADD is the escape hatch apply() already handles).
	// The cluster shares one chat_id by construction, so this keeps a merged
	// memory addressable by the same outcome-feedback event as the originals.
	prov := Provenance{ChatID: cluster[0].ChatID, NodeID: cluster[0].NodeID, Source: cluster[0].Source}
	return s.apply(ctx, bucket, consolidatorAuthor, prov, ops, valid)
}

// retentionOnce hard-deletes invalidated points and memory_ops rows older
// than retentionDays (design doc §6's bound on unbounded growth). <= 0 keeps
// everything forever - a true no-op, not just a skipped delete, so a
// misconfigured zero can never silently wipe history. No per-point log: the
// deleted rows are themselves the audit trail aging out; only a summary logs.
func (s *Store) retentionOnce(ctx context.Context, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	pts, err := s.idx.list(ctx, nil, 0, 0, true) // every bucket, including invalidated
	if err != nil {
		s.log.Warn("retention sweep: list failed", "err", err)
		return
	}
	var expired []string
	for _, p := range pts {
		if p.Status != string(StatusInvalidated) || p.InvalidatedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, p.InvalidatedAt)
		if err != nil || t.After(cutoff) {
			continue
		}
		expired = append(expired, p.ID)
	}
	removed := 0
	if len(expired) > 0 {
		n, err := s.idx.remove(ctx, expired)
		if err != nil {
			s.log.Warn("retention sweep: point removal failed", "err", err)
		} else {
			removed = n
		}
	}

	prunedOps := 0
	if s.opsLog != nil {
		n, err := s.opsLog.PruneMemoryOps(ctx, cutoff)
		if err != nil {
			s.log.Warn("retention sweep: memory_ops prune failed", "err", err)
		} else {
			prunedOps = n
		}
	}
	s.log.Info("retention sweep", "points_removed", removed, "ops_pruned", prunedOps)
}
