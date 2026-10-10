package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/fagerbergj/quack/internal/memoryrules"
)

// clusterWindow chains the burst-dedupe cluster: consecutive unverified memories from one chat_id
// minted no more than this apart join one cluster.
const clusterWindow = 15 * time.Minute

// consolidatorAuthor tags a point the sweep itself writes (an UPDATE merging
// a burst's wording), distinct from the agent name a live commit stamps.
const consolidatorAuthor = "memory-consolidator"

// sweepPageSize bounds each list() call the sweep makes to the backend, so a
// growing collection can't force one unbounded scroll/select per tick.
const sweepPageSize = 500

// forEachSweepPage walks every point across all buckets in pages of sweepPageSize. withVectors
// adds each point's stored embedding (for DedupeSweep), never a re-embed.
func (s *Store) forEachSweepPage(ctx context.Context, includeInvalidated, withVectors bool, fn func([]scored)) error {
	if s.listErrForTest != nil {
		return s.listErrForTest
	}
	return s.idx.scrollAll(ctx, includeInvalidated, withVectors, sweepPageSize, fn)
}

// RunConsolidationSweep runs the sweep on a 5-field cron schedule until ctx is done; "" disables it.
// Never runs at boot: it waits for the first Next.
func (s *Store) RunConsolidationSweep(ctx context.Context, schedule string, retentionDays int) {
	if schedule == "" {
		return
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		s.log.Warn("consolidation sweep: invalid schedule, sweep disabled", "schedule", schedule, "err", err)
		return
	}
	for {
		timer := time.NewTimer(time.Until(sched.Next(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.sweepOnce(ctx, retentionDays)
		}
	}
}

func (s *Store) sweepOnce(ctx context.Context, retentionDays int) {
	s.consolidateOnce(ctx)
	// Burst clustering only compares one chat within 15 minutes; this bucket-wide pass catches a fact
	// a different run re-derived days later.
	if _, err := s.DedupeSweep(ctx, true); err != nil {
		s.log.Warn("dedupe sweep failed", "err", err)
	}
	s.forgetOnce(ctx, false)
	s.retentionOnce(ctx, retentionDays)
}

// consolidateOnce clusters valid unverified memories by bucket + chat_id + time proximity and asks the
// consolidation model to dedupe each cluster. Ticker-only, never on the commit path.
func (s *Store) consolidateOnce(ctx context.Context) {
	// Clustering needs every valid unverified memory per bucket, so accumulation is unavoidable;
	// paging still bounds each backend call.
	byBucket := map[string][]scored{}
	err := s.forEachSweepPage(ctx, false, false, func(page []scored) { // currently-valid only
		for _, p := range page {
			if p.Status == string(StatusReinforced) {
				continue // earned trust; never a dedupe candidate
			}
			byBucket[p.Scope] = append(byBucket[p.Scope], p)
		}
	})
	if err != nil {
		s.log.Warn("consolidation sweep: list failed", "err", err)
		return
	}
	clusters, skipped, called, applied := 0, 0, 0, 0
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) { // deterministic order
		for _, cluster := range burstClusters(byBucket[bucket]) {
			clusters++
			fp := s.clusterFingerprint(cluster)
			if clusterFingerprintMatches(cluster, fp) {
				// Same members, same wording as the sweep that already judged this
				// burst a pure no-op - re-asking the model can only repeat that answer.
				skipped++
				continue
			}
			called++
			n, err := s.consolidateCluster(ctx, bucket, cluster)
			if err != nil {
				s.log.Warn("consolidation sweep: cluster failed", "bucket", bucket, "err", err)
				continue
			}
			applied += n
			if n == 0 {
				s.stampClusterNoChange(ctx, cluster, fp)
			}
		}
	}
	s.log.Info("consolidation sweep", "clusters", clusters, "skipped", skipped, "called", called, "ops_applied", applied)
}

// clusterFingerprint must hash exactly what buildDedupePrompt sends (id+content), salted with prompt
// and model so a deploy that changes either doesn't keep skipping on the old verdict.
func (s *Store) clusterFingerprint(cluster []scored) string {
	keys := make([]string, len(cluster))
	for i, p := range cluster {
		keys[i] = p.ID + "\x00" + p.Content
	}
	sort.Strings(keys)
	sysPrompt, ok := consolidateDedupePrompts[s.domain]
	if !ok {
		sysPrompt = consolidateDedupePrompts["task"]
	}
	modelName := ""
	if s.consolidator != nil {
		modelName = s.consolidator.Name()
	}
	h := sha256.New()
	h.Write([]byte(sysPrompt))
	h.Write([]byte{0x01})
	h.Write([]byte(modelName))
	for _, k := range keys {
		h.Write([]byte{0x01})
		h.Write([]byte(k))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// clusterFingerprintMatches reports whether the last sweep already judged
// this exact burst (same fp) a pure no-op.
func clusterFingerprintMatches(cluster []scored, fp string) bool {
	for _, p := range cluster {
		if p.ConsolidateFP != fp {
			return false
		}
	}
	return true
}

// stampClusterNoChange records fp after a writes=0 result - apply() never
// touches a NOOP'd point, so nothing else persists this.
func (s *Store) stampClusterNoChange(ctx context.Context, cluster []scored, fp string) {
	ids := make([]string, len(cluster))
	for i, p := range cluster {
		ids[i] = p.ID
	}
	if err := s.idx.patch(ctx, ids, map[string]any{payloadConsolidateFP: fp}); err != nil {
		s.log.Warn("consolidation sweep: fingerprint stamp failed", "err", err)
	}
}

// burstClusters groups one bucket's unverified pts by ChatID, chained by MintedAt gaps <= clusterWindow,
// returning clusters of >=2. Points with no ChatID or an unparsable MintedAt are dropped, not guessed.
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

// consolidateCluster runs the dedupe prompt over one burst and applies its ops; the cluster is both
// candidates and neighbours, so "duplicate of <id>" always names a shown id.
func (s *Store) consolidateCluster(ctx context.Context, bucket string, cluster []scored) (int, error) {
	neighbours := make([]neighbour, len(cluster))
	valid := make(map[string]neighbour, len(cluster))
	for i, p := range cluster {
		n := p.toNeighbour()
		neighbours[i] = n
		valid[p.ID] = n
	}
	ops, err := s.decideDedupe(ctx, neighbours)
	if err != nil {
		return 0, err
	}
	// Provenance for a fallback ADD: the cluster shares one chat_id, so a merged memory stays
	// addressable by the same outcome-feedback event as the originals.
	prov := Provenance{ChatID: cluster[0].ChatID, NodeID: cluster[0].NodeID, Source: cluster[0].Source}
	return s.apply(ctx, bucket, consolidatorAuthor, prov, ops, valid)
}

// forgetExampleCap bounds how many example ids/contents a dry-run report
// carries per rule - enough to sanity-check a rule, not a full dump.
const forgetExampleCap = 5

// ForgettingExample is one matched memory shown in a dry-run report.
type ForgettingExample struct {
	ID      string
	Content string
}

// ForgettingRuleResult is one rule's outcome across the sweep.
type ForgettingRuleResult struct {
	Index    int
	When     string
	Then     string
	Matched  int
	Examples []ForgettingExample
}

// ForgettingReport summarizes one ForgetSweep, as logged nightly or printed by `quack memory sweep`.
type ForgettingReport struct {
	Evaluated int
	Kept      int // no rule matched
	Rules     []ForgettingRuleResult
}

// forgetOnce runs before retentionOnce, so a memory a rule invalidates this tick ages toward the
// retention cutoff from now on.
func (s *Store) forgetOnce(ctx context.Context, dryRun bool) {
	report, err := s.ForgetSweep(ctx, dryRun)
	if err != nil {
		s.log.Warn("forgetting sweep failed", "err", err)
		return
	}
	for _, r := range report.Rules {
		s.log.Info("forgetting sweep rule", "rule", r.Index, "then", r.Then, "matched", r.Matched)
	}
	s.log.Info("forgetting sweep", "evaluated", report.Evaluated, "kept", report.Kept, "dry_run", dryRun)
}

// ForgetSweep applies the forgetting rules (first match wins, none keeps) to every valid memory; dryRun
// only reports. Shared by the nightly sweep and the CLI. Votes may change mid-sweep: last write wins.
func (s *Store) ForgetSweep(ctx context.Context, dryRun bool) (ForgettingReport, error) {
	rules := memoryrules.DefaultRules()
	report := ForgettingReport{Rules: make([]ForgettingRuleResult, len(rules))}
	for i, r := range rules {
		report.Rules[i] = ForgettingRuleResult{Index: i, When: r.When, Then: r.Then}
	}

	var toInvalidate []sweepHit
	var toDemote []string
	now := time.Now().UTC()
	err := s.forEachSweepPage(ctx, false, false, func(page []scored) { // currently-valid only
		for _, p := range page {
			report.Evaluated++
			matched := matchForgetRule(p, now, rules, &report)
			if matched < 0 {
				report.Kept++
				continue
			}
			switch rules[matched].Then {
			case memoryrules.ThenInvalidate:
				toInvalidate = append(toInvalidate, sweepHit{id: p.ID, rule: matched})
			case memoryrules.ThenDemote:
				toDemote = append(toDemote, p.ID)
			}
		}
	})
	if err != nil {
		return report, fmt.Errorf("memory: forgetting sweep: list: %w", err)
	}
	if dryRun {
		return report, nil
	}
	if len(toInvalidate) > 0 {
		s.invalidateByRule(ctx, toInvalidate, rules)
	}
	if len(toDemote) > 0 {
		s.demoteByRule(ctx, toDemote)
	}
	return report, nil
}

// sweepHit is one memory a forgetting rule invalidated: its id plus the
// matched rule's index (the invalidate reason is built from it).
type sweepHit struct {
	id   string
	rule int
}

// matchForgetRule records the first matching rule for p (count plus capped examples); -1 if none.
func matchForgetRule(p scored, now time.Time, rules []memoryrules.Rule, report *ForgettingReport) int {
	f := fieldsFor(p, now)
	for i, r := range rules {
		if r.Match(f) {
			rr := &report.Rules[i]
			rr.Matched++
			if len(rr.Examples) < forgetExampleCap {
				rr.Examples = append(rr.Examples, ForgettingExample{ID: p.ID, Content: preview(p.Content)})
			}
			return i
		}
	}
	return -1
}

// invalidateByRule soft-invalidates the sweep's hits grouped by rule, one memory_ops row per id.
func (s *Store) invalidateByRule(ctx context.Context, toInvalidate []sweepHit, rules []memoryrules.Rule) {
	byRule := map[int][]string{}
	for _, h := range toInvalidate {
		byRule[h.rule] = append(byRule[h.rule], h.id)
	}
	for rule, ids := range byRule {
		reason := rules[rule].Reason
		if reason == "" {
			reason = fmt.Sprintf("rule %d: %s", rule, rules[rule].When)
		}
		if _, err := s.invalidateByID(ctx, ids, reason); err != nil {
			s.log.Warn("forgetting sweep: invalidate failed", "rule", rule, "err", err)
			continue
		}
		for _, id := range ids {
			s.logOp(ctx, id, OpInvalidate, ActorSweep, reason)
		}
	}
}

// demoteByRule demotes the sweep's demote hits, one memory_ops row per id actually changed,
// always with ReasonSupportDecayed.
func (s *Store) demoteByRule(ctx context.Context, ids []string) {
	touched, err := s.demoteTier(ctx, ids)
	if err != nil {
		s.log.Warn("forgetting sweep: demote failed", "err", err)
		return
	}
	for _, id := range touched {
		s.logOp(ctx, id, OpDemote, ActorConsolidator, ReasonSupportDecayed)
	}
}

// fieldsFor snapshots p for rule evaluation; never-upvoted falls back to MintedAt's age. Ages use
// float Hours()/24 so a zero or malformed timestamp can't overflow into a bogus age.
func fieldsFor(p scored, now time.Time) memoryrules.Fields {
	daysSinceUpvote := ageInDays(p.MintedAt, now)
	if p.LastUpvotedAt != "" {
		daysSinceUpvote = ageInDays(p.LastUpvotedAt, now)
	}
	// Legacy row predating MintedAt: ValidFrom is preserved across an UPDATE (unlike Timestamp,
	// which a consolidator reword re-stamps to now), so it's the safer fallback.
	mintedAt := p.MintedAt
	if mintedAt == "" {
		mintedAt = p.ValidFrom
	}
	if mintedAt == "" {
		mintedAt = p.Timestamp
	}
	tier := p.Tier
	if tier == "" {
		tier = TierUnverified
	}
	return memoryrules.Fields{
		Supported: p.Supported, Score: p.VoteScore, Recalls: p.Recalls,
		DaysSinceUpvote: daysSinceUpvote, DaysSinceMinted: ageInDays(mintedAt, now), Tier: tier,
	}
}

// ageInDays is an RFC3339 timestamp's age in whole days, floored at 0; unparsable reads as 0
// rather than failing the sweep over one bad row.
func ageInDays(ts string, now time.Time) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	days := int64(now.Sub(t).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// retentionOnce hard-deletes invalidated points and memory_ops rows older than retentionDays.
// <= 0 is a true no-op, so a misconfigured zero can never wipe history.
func (s *Store) retentionOnce(ctx context.Context, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	// Accumulate ids only and delete after the walk: deleting mid-walk would shift later
	// pages and skip still-expired rows.
	var expired []string
	err := s.forEachSweepPage(ctx, true, false, func(page []scored) { // every bucket, including invalidated
		for _, p := range page {
			if p.Status != string(StatusInvalidated) || p.InvalidatedAt == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, p.InvalidatedAt)
			if err != nil || t.After(cutoff) {
				continue
			}
			expired = append(expired, p.ID)
		}
	})
	if err != nil {
		s.log.Warn("retention sweep: list failed", "err", err)
		return
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
