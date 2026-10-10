package memory

import (
	"context"
	"maps"
	"slices"
	"sort"
)

// dedupeCosineThreshold is the near-duplicate bar for the per-bucket sweep, the bar the measured
// duplicate rate was sized with (29 live pairs >= 0.90 across two buckets).
const dedupeCosineThreshold = 0.90

// dedupeMaxClusterSize bounds transitive chaining so a blob of similar memories can't force one
// giant consolidation prompt.
const dedupeMaxClusterSize = 12

// dedupeMaxLLMCalls caps clusters sent to the model per sweep, so a backlog (e.g. after a rescope)
// can't spike one tick's cost; the rest wait for the next sweep.
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

// DedupeSweep clusters each bucket's live memories by stored-vector cosine, catching duplicates the burst
// sweep misses across runs, and with apply merges each cluster via consolidateCluster (<= dedupeMaxLLMCalls).
func (s *Store) DedupeSweep(ctx context.Context, apply bool) (DedupeReport, error) {
	byBucket := map[string][]scored{}
	err := s.forEachSweepPage(ctx, false, true, func(page []scored) { // currently-valid only, with vectors
		for _, p := range page {
			byBucket[p.Scope] = append(byBucket[p.Scope], p)
		}
	})
	if err != nil {
		return DedupeReport{}, err
	}

	report := DedupeReport{Applied: apply}
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) { // deterministic order
		for _, cluster := range cosineClusters(byBucket[bucket], dedupeCosineThreshold, dedupeMaxClusterSize) {
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

// cosineClusters transitively unions pts at >= threshold (size <= maxSize, only >= 2 returned), ordering
// highest-VoteScore-then-oldest first so upvoted memories survive. ponytail: O(n²) per bucket, ANN if it lags.
func cosineClusters(pts []scored, threshold float32, maxSize int) [][]scored {
	n := len(pts)
	parent := make([]int, n)
	size := make([]int, n)
	for i := range parent {
		parent[i] = i
		size[i] = 1
	}
	find := func(x int) int {
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
			if cosine(pts[i].Vector, pts[j].Vector) >= threshold {
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
		sort.Slice(g, func(i, j int) bool {
			if g[i].VoteScore != g[j].VoteScore {
				return g[i].VoteScore > g[j].VoteScore // highest-voted first: preferred survivor
			}
			if g[i].MintedAt != g[j].MintedAt {
				return g[i].MintedAt < g[j].MintedAt // then oldest first
			}
			return g[i].ID < g[j].ID // deterministic tiebreak
		})
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].ID < out[j][0].ID }) // deterministic cluster order
	return out
}
