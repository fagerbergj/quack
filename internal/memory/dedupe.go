package memory

import (
	"context"
	"maps"
	"slices"
	"sort"
)

// dedupeCosineThreshold is the near-duplicate bar for the per-bucket sweep
// (issue #1269): the same bar the pre-implementation measurement used to
// size the duplicate rate (29 live pairs >= 0.90 across two buckets).
const dedupeCosineThreshold = 0.90

// dedupeMaxClusterSize bounds one cluster's size (transitive cosine
// chaining otherwise has no ceiling) so a degenerate blob of mutually
// similar memories can't force one giant consolidation prompt.
const dedupeMaxClusterSize = 12

// dedupeMaxLLMCalls caps how many clusters one DedupeSweep(apply=true) run
// will send to the consolidation model, so a large backlog (e.g. right
// after a rescope) can't spike one sweep tick's cost unboundedly. Clusters
// beyond the cap are simply left for the next sweep.
const dedupeMaxLLMCalls = 30

// dedupeExampleCap bounds how many members of a cluster the report carries -
// enough to sanity-check a cluster, not a full dump.
const dedupeExampleCap = 5

// dedupeReportClusterCap bounds how many clusters the report lists in full -
// NumClusters/LLMCalls/Dropped still count every one.
const dedupeReportClusterCap = 20

// DedupeExample is one cluster member shown in a report.
type DedupeExample struct {
	ID      string
	Content string
}

// DedupeCluster is one similarity cluster the sweep found.
type DedupeCluster struct {
	Bucket   string
	Size     int
	Examples []DedupeExample // capped at dedupeExampleCap
}

// DedupeReport is DedupeSweep's result - `quack memory sweep --dedupe`
// (dry run) or `--dedupe --apply` prints it.
type DedupeReport struct {
	Applied     bool
	NumClusters int
	Clusters    []DedupeCluster // capped at dedupeReportClusterCap
	LLMCalls    int
	OpsApplied  int
	Dropped     int // clusters skipped once LLMCalls hit dedupeMaxLLMCalls
}

// DedupeSweep clusters every bucket's live, non-reinforced memories by
// cosine similarity (issue #1269): the burst sweep only ever compares
// memories minted by the same chat within a 15-minute window, so a fact
// re-derived independently by a different run - even days later - never
// gets compared. This pass re-embeds every candidate (one batch call per
// bucket) and clusters transitively at >= dedupeCosineThreshold, then feeds
// each cluster of size >= 2 to the same consolidation model the burst sweep
// uses (consolidateCluster, with P5 lineage: the survivor keeps summed
// votes, the absorbed is invalidated "absorbed by <id>").
//
// apply=false only clusters and reports - no LLM call, no write. apply=true
// runs consolidation (bounded by dedupeMaxLLMCalls) and applies its ops.
func (s *Store) DedupeSweep(ctx context.Context, apply bool) (DedupeReport, error) {
	byBucket := map[string][]scored{}
	err := s.forEachSweepPage(ctx, false, func(page []scored) { // currently-valid only
		for _, p := range page {
			if p.Status == string(StatusReinforced) {
				continue // earned trust; never a dedupe candidate
			}
			byBucket[p.Scope] = append(byBucket[p.Scope], p)
		}
	})
	if err != nil {
		return DedupeReport{}, err
	}

	report := DedupeReport{Applied: apply}
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) { // deterministic order
		pts := byBucket[bucket]
		vecs, err := s.embedContents(ctx, pts)
		if err != nil {
			s.log.Warn("dedupe sweep: embed failed", "bucket", bucket, "err", err)
			continue
		}
		for _, cluster := range cosineClusters(pts, vecs, dedupeCosineThreshold, dedupeMaxClusterSize) {
			report.NumClusters++
			if len(report.Clusters) < dedupeReportClusterCap {
				report.Clusters = append(report.Clusters, clusterReport(bucket, cluster))
			}
			if !apply {
				continue
			}
			if report.LLMCalls >= dedupeMaxLLMCalls {
				report.Dropped++
				continue
			}
			report.LLMCalls++
			n, err := s.consolidateCluster(ctx, bucket, cluster)
			if err != nil {
				s.log.Warn("dedupe sweep: cluster failed", "bucket", bucket, "err", err)
				continue
			}
			report.OpsApplied += n
		}
	}
	s.log.Info("dedupe sweep", "clusters", report.NumClusters, "llm_calls", report.LLMCalls,
		"dropped", report.Dropped, "ops_applied", report.OpsApplied, "applied", apply)
	return report, nil
}

// embedContents batch-embeds pts' content in one call, same order as pts.
func (s *Store) embedContents(ctx context.Context, pts []scored) ([][]float32, error) {
	texts := make([]string, len(pts))
	for i, p := range pts {
		texts[i] = p.Content
	}
	return s.embed(ctx, texts, "dedupe-sweep")
}

func clusterReport(bucket string, cluster []scored) DedupeCluster {
	c := DedupeCluster{Bucket: bucket, Size: len(cluster)}
	for i, p := range cluster {
		if i >= dedupeExampleCap {
			break
		}
		c.Examples = append(c.Examples, DedupeExample{ID: p.ID, Content: preview(p.Content)})
	}
	return c
}

// cosineClusters unions pts[i]/pts[j] whenever their vectors' cosine
// similarity is >= threshold (transitive: a-b and b-c above threshold puts
// a, b, c in one cluster even if a-c is below it), bounded to maxSize per
// cluster. Only clusters of size >= 2 are returned.
//
// ponytail: O(n²) pairwise cosine per bucket - fine at memory's documented
// scale (hundreds-thousands per bucket, run nightly off the hot path);
// revisit with an ANN index only if a bucket's dedupe pass measurably lags.
func cosineClusters(pts []scored, vecs [][]float32, threshold float32, maxSize int) [][]scored {
	n := len(pts)
	parent := make([]int, n)
	size := make([]int, n)
	for i := range parent {
		parent[i] = i
		size[i] = 1
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb || size[ra]+size[rb] > maxSize {
			return
		}
		parent[ra] = rb
		size[rb] += size[ra]
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cosine(vecs[i], vecs[j]) >= threshold {
				union(i, j)
			}
		}
	}

	groups := map[int][]scored{}
	for i, p := range pts {
		r := find(i)
		groups[r] = append(groups[r], p)
	}
	var out [][]scored
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].ID < g[j].ID })
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].ID < out[j][0].ID }) // deterministic order
	return out
}
