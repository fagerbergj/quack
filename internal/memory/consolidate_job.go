package memory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
)

// clusterWindow is the temporal-proximity threshold the burst-dedupe cluster
// (design doc §4(c)) chains on: consecutive unverified memories from the same
// chat_id, minted no more than this apart, join one cluster.
const clusterWindow = 15 * time.Minute

// consolidatorAuthor tags a point the sweep itself writes (an UPDATE merging
// a burst's wording), distinct from the agent name a live commit stamps.
const consolidatorAuthor = "memory-consolidator"

// sweepPageSize bounds each list() call the sweep makes to the backend, so a
// growing collection can't force one unbounded scroll/select per tick.
const sweepPageSize = 500

// forEachSweepPage walks every point across all buckets in pages of
// sweepPageSize, calling fn once per page until the backend is exhausted.
// withVectors asks the backend to also populate each point's stored
// embedding (DedupeSweep's cosine clustering, issue #1269) - both backends
// already have the vector on hand at list time, so this is never a
// re-embed, just an extra field on the same read.
func (s *Store) forEachSweepPage(ctx context.Context, includeInvalidated, withVectors bool, fn func([]scored)) error {
	if s.listErrForTest != nil {
		return s.listErrForTest
	}
	for offset := 0; ; offset += sweepPageSize {
		page, err := s.idx.list(ctx, nil, offset, sweepPageSize, includeInvalidated, "", withVectors)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		fn(page)
		if len(page) < sweepPageSize {
			return nil
		}
	}
}

// RunConsolidationSweep runs the sweep (design doc §4(c), §6) on schedule (a
// standard 5-field cron expression), until ctx is done. schedule == ""
// disables it. Never runs at boot (issue #961) - waits for the first Next.
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
	// Per-bucket similarity dedupe (issue #1269): consolidateOnce's burst
	// clustering only ever compares memories from the same chat within a
	// 15-minute window, so a fact re-derived by a different run days later
	// is never caught there - this pass catches it, bucket-wide.
	if _, err := s.DedupeSweep(ctx, true); err != nil {
		s.log.Warn("dedupe sweep failed", "err", err)
	}
	s.forgetOnce(ctx, false)
	s.retentionOnce(ctx, retentionDays)
}

// consolidateOnce clusters currently-valid unverified memories by bucket +
// provenance chat_id + temporal proximity and asks the consolidation model to
// dedupe each cluster (design doc §4(c)). Off the hot path: reached only from
// the ticker, never inlined in a commit.
func (s *Store) consolidateOnce(ctx context.Context) {
	// Clustering needs every currently-valid unverified memory grouped by
	// bucket before it can chain bursts, so the accumulation itself isn't
	// avoidable here - but paging the fetch still bounds each backend call
	// (vs. one unbounded scroll/select) as the collection grows.
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
			Upvotes: p.Upvotes, Downvotes: p.Downvotes, VoteScore: p.VoteScore, Tier: p.Tier,
			LastUpvotedAt: p.LastUpvotedAt, Recalls: p.Recalls, LastRecalledAt: p.LastRecalledAt,
			AbsorbedIDs: p.AbsorbedIDs,
		}
		neighbours[i] = n
		valid[p.ID] = n
	}
	ops, err := s.decideDedupe(ctx, neighbours)
	if err != nil {
		return 0, err
	}
	// Provenance for a fallback fresh ADD (the dedupe prompt asks for
	// UPDATE/DELETE/NOOP; ADD is the escape hatch apply() already handles).
	// The cluster shares one chat_id by construction, so this keeps a merged
	// memory addressable by the same outcome-feedback event as the originals.
	prov := Provenance{ChatID: cluster[0].ChatID, NodeID: cluster[0].NodeID, Source: cluster[0].Source}
	return s.apply(ctx, bucket, consolidatorAuthor, prov, ops, valid)
}

// forgetExampleCap bounds how many example ids/contents a dry-run report
// carries per rule - enough to sanity-check a rule, not a full dump.
const forgetExampleCap = 5

// SetForgettingRules validates and wires the operator's memory.forgetting.rules
// (epic #1255 P3). Called once at server startup - a bad rule fails fast
// there rather than surfacing later as a silently-skipped nightly sweep.
// Unset (nil rules, never called) means DefaultRules().
func (s *Store) SetForgettingRules(rules []Rule) error {
	if err := ValidateRules(rules); err != nil {
		return err
	}
	s.forgetRules = rules
	return nil
}

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

// ForgettingReport summarizes one ForgetSweep call - the nightly sweep logs
// it, `quack memory sweep --dry-run` prints it, `quack memory sweep` prints
// it after applying.
type ForgettingReport struct {
	Evaluated int
	Kept      int // no rule matched
	Rules     []ForgettingRuleResult
}

// forgetOnce is the sweep's forgetting step (epic #1255 P3), run before
// retentionOnce so a memory a rule invalidates this tick is also eligible
// for the same tick's retention cutoff check next run.
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

// ForgetSweep evaluates every currently-valid memory against the configured
// (or default) forgetting rules, first match wins, no match keeps. dryRun
// reports what would happen without mutating anything; otherwise matched
// "invalidate" memories are soft-invalidated with reason "rule <index>: <expr>",
// same sticky soft-delete every other invalidation path uses. This is the
// ONE code path both the nightly sweep and `quack memory sweep` call -
// no duplicated sweep logic.
//
// Concurrency: a point's votes can change between this read and the
// invalidate write below; accepted as eventual consistency, last-write-wins,
// same as every other invalidateByID caller - no new locking is introduced.
func (s *Store) ForgetSweep(ctx context.Context, dryRun bool) (ForgettingReport, error) {
	rules := s.forgetRules
	if len(rules) == 0 {
		rules = DefaultRules()
	}
	report := ForgettingReport{Rules: make([]ForgettingRuleResult, len(rules))}
	for i, r := range rules {
		report.Rules[i] = ForgettingRuleResult{Index: i, When: r.When, Then: r.Then}
	}

	type hit struct {
		id   string
		rule int
	}
	var toInvalidate []hit
	now := time.Now().UTC()
	err := s.forEachSweepPage(ctx, false, false, func(page []scored) { // currently-valid only
		for _, p := range page {
			report.Evaluated++
			f := fieldsFor(p, now)
			matched := -1
			for i, r := range rules {
				ok, err := Evaluate(r.When, f)
				if err != nil {
					s.log.Warn("forgetting sweep: rule evaluation failed, treating as no-match", "rule", i, "err", err)
					continue
				}
				if ok {
					matched = i
					break
				}
			}
			if matched < 0 {
				report.Kept++
				continue
			}
			rr := &report.Rules[matched]
			rr.Matched++
			if len(rr.Examples) < forgetExampleCap {
				rr.Examples = append(rr.Examples, ForgettingExample{ID: p.ID, Content: preview(p.Content)})
			}
			if rules[matched].Then == ThenInvalidate {
				toInvalidate = append(toInvalidate, hit{id: p.ID, rule: matched})
			}
		}
	})
	if err != nil {
		return report, fmt.Errorf("memory: forgetting sweep: list: %w", err)
	}
	if dryRun || len(toInvalidate) == 0 {
		return report, nil
	}
	byRule := map[int][]string{}
	for _, h := range toInvalidate {
		byRule[h.rule] = append(byRule[h.rule], h.id)
	}
	for rule, ids := range byRule {
		reason := fmt.Sprintf("rule %d: %s", rule, rules[rule].When)
		if _, err := s.idx.invalidateByID(ctx, ids, reason); err != nil {
			s.log.Warn("forgetting sweep: invalidate failed", "rule", rule, "err", err)
			continue
		}
		for _, id := range ids {
			s.logOp(ctx, id, OpInvalidate, ActorSweep, reason)
		}
	}
	return report, nil
}

// fieldsFor computes a point's Fields snapshot for forgetting-rule
// evaluation. Missing timestamps (never upvoted/recalled) evaluate as
// "never": days_since_upvote/days_since_recall fall back to age_days.
// All timestamps are RFC3339 UTC (see nowRFC3339) - age math stays in UTC
// throughout, and uses float Hours()/24 rather than integer subtraction so a
// zero-value or malformed timestamp can't overflow into a bogus age.
func fieldsFor(p scored, now time.Time) Fields {
	ageDays := ageInDays(p.MintedAt, now)
	daysSinceUpvote := ageDays
	if p.LastUpvotedAt != "" {
		daysSinceUpvote = ageInDays(p.LastUpvotedAt, now)
	}
	daysSinceRecall := ageDays
	if p.LastRecalledAt != "" {
		daysSinceRecall = ageInDays(p.LastRecalledAt, now)
	}
	tier := p.Tier
	if tier == "" {
		tier = TierUnverified
	}
	return Fields{
		Upvotes: p.Upvotes, Downvotes: p.Downvotes, Score: p.VoteScore,
		AgeDays: ageDays, DaysSinceUpvote: daysSinceUpvote, DaysSinceRecall: daysSinceRecall,
		Recalls: p.Recalls, Tier: tier, Scope: p.Scope,
	}
}

// ageInDays is the age of an RFC3339 timestamp in whole days, floored at 0.
// An unparsable or empty timestamp reads as 0 (brand new) rather than
// erroring the sweep over one bad row.
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

	// Page the fetch, but only ever accumulate ids (not full points - the
	// bulk of a point's size is Content), and remove after the walk
	// completes rather than mid-page: deleting during an offset-paged walk
	// would shift a later page's window and skip a still-expired row (it
	// would just be caught on the next tick, but there's no reason to risk it).
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
