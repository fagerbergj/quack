// Package memory is Quack's semantic-memory layer over a swappable vector index.
package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/memoryrules"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
)

// index is the vector-storage backend; Store holds the shared logic.
type index interface {
	// ensure makes the backing collection/table ready. probeDim returns the embedding dimension.
	ensure(ctx context.Context, probeDim func() (int, error)) error
	// query returns up to k points in any of the given buckets, nearest by cosine.
	query(ctx context.Context, buckets []string, vec []float32, k int) ([]scored, error)
	// list pages points in buckets (all if empty) by sortBy[0] (a ListSort, default newest), ordering the whole
	// matching set before offset/limit. limit<=0 = no cap; "unverified" tier also matches a missing tier.
	list(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool, tier string, withVectors bool, sortBy ...string) ([]scored, error)
	// scrollAll visits every point across all buckets once, in unsorted pages of pageSize, letting
	// an implementation thread one native cursor through the walk.
	scrollAll(ctx context.Context, includeInvalidated, withVectors bool, pageSize int, fn func([]scored)) error
	// count returns how many points match buckets (all buckets if empty), under the
	// same includeInvalidated/tier filter as list.
	count(ctx context.Context, buckets []string, includeInvalidated bool, tier string) (int, error)
	upsert(ctx context.Context, pts []point) error
	// getByID fetches one point by id, invalidated or not; ok=false if it doesn't exist.
	getByID(ctx context.Context, id string) (pt scored, ok bool, err error)
	// remove deletes the named ids and reports how many actually existed.
	remove(ctx context.Context, ids []string) (int, error)
	// invalidateByID soft-invalidates ids in place (never removes) and reports how many existed.
	invalidateByID(ctx context.Context, ids []string, reason string) (int, error)
	// updateStatus applies o to every non-invalidated id (sticky) and returns those touched; invalidation
	// skips verified ones, since a verified memory recalled into a closed-unmerged chat gets no vote.
	updateStatus(ctx context.Context, ids []string, o OutcomeSignal) ([]string, error)
	// applyVotes applies each vote to a non-invalidated memory and returns the ids touched; a net
	// score <= invalidateThreshold soft-invalidates it (OutcomeReasonNetScore).
	applyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) ([]string, error)
	// recordRecall bumps recalls and stamps last_recalled_at for ids, one
	// batched write - the usage-tracking half of a recall delivery.
	recordRecall(ctx context.Context, ids []string) error
	// backfillTiers gives every tierless point a tier (verified iff reinforcement_count >= 1). Idempotent.
	backfillTiers(ctx context.Context) (int, error)
	// backfillJudgeSupport sets supported = upvotes - reinforcement_count on verified points still at 0,
	// demoting them when that is not positive. Idempotent both ways.
	backfillJudgeSupport(ctx context.Context) (int, error)
	// updateBucket moves one point to a new bucket key; no re-embed.
	updateBucket(ctx context.Context, id, bucket string) error
	// absorb folds absorbedID's votes/timestamps/lineage into survivorID and invalidates absorbedID.
	// False (no-op) if either is missing or absorbedID is already invalidated (first invalidation wins).
	absorb(ctx context.Context, survivorID, absorbedID, reason string) (bool, error)
	// setHumanVote sets the caller's vote on id, deriving counts from the transition away from the prior
	// human_vote so a toggle never double counts. Reports whether id existed and wasn't invalidated.
	setHumanVote(ctx context.Context, id, vote string, invalidateThreshold int) (bool, error)
	// stampConsolidateFP records fp as ids' consolidate fingerprint (payload
	// only) - the sweep's skip check reads it back next tick.
	stampConsolidateFP(ctx context.Context, ids []string, fp string) error
	// demoteTier sets tier=unverified on every verified id and returns only those it changed,
	// so the caller logs no memory_ops row for an already-unverified id.
	demoteTier(ctx context.Context, ids []string) ([]string, error)
}

// scored is one ranked memory.
type scored struct {
	ID        string
	Content   string
	Author    string
	Timestamp string
	Kind      string
	Scope     string // the bucket this point is stored under
	ChatID    string // provenance: minting chat (see Provenance)
	NodeID    string // provenance: minting DAG node, empty for an orchestrator-level commit
	Source    string // provenance: minting run's origin, empty = native quack run
	MintedAt  string // set once on ADD, never changed by an UPDATE
	// Empty Status reads as StatusUnverified everywhere (recall filter, tier prefix).
	Status             string
	ValidFrom          string
	InvalidatedAt      string
	InvalidationReason string
	ReinforcementCount int
	Score              float32

	// Votes are independent of Score (cosine rank). Supported is the judge/human-backed subset of Upvotes
	// (reinforcement doesn't count); Tier is "verified" only while Supported >= 1, recomputed per vote.
	Upvotes        int
	Downvotes      int
	Supported      int
	NotRelevant    int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string

	// AbsorbedIDs lists memories consolidation merged into this one, flattened across chains (lineage.go).
	AbsorbedIDs []string
	// HumanVote is the deployment user's own vote ("up"/"down"/""), separate from the mixed judge+human
	// Upvotes/Downvotes; toggling re-derives the delta from it.
	HumanVote string
	// ConsolidateFP is the burst fingerprint the sweep last judged a pure
	// no-op for this point ("" if never stamped or cleared by a write).
	ConsolidateFP string
	// Vector is set by query() only, for recall's MMR re-rank (inter-hit cosine).
	Vector []float32
}

// point is one memory to upsert.
type point struct {
	ID        string
	Vector    []float32
	Content   string
	Scope     string
	Author    string
	Timestamp string
	Kind      string
	ChatID    string
	NodeID    string
	Source    string
	MintedAt  string

	Status             string
	ValidFrom          string
	InvalidatedAt      string
	InvalidationReason string
	ReinforcementCount int

	Upvotes        int
	Downvotes      int
	Supported      int
	NotRelevant    int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string

	// AbsorbedIDs: see scored.AbsorbedIDs.
	AbsorbedIDs []string
}

const (
	// maxRecallRunes bounds recall query size so one oversized input can't stall the embedder.
	maxRecallRunes = 2000
	// recallEmbedTimeout bounds how long recall waits before degrading to no-recall.
	recallEmbedTimeout = 30 * time.Second
	// recallFetchMultiplier over-fetches topK so mmrSelect has room to swap a near-duplicate for a distinct hit.
	recallFetchMultiplier = 2
	// recallDiversityThreshold is the near-duplicate cosine bar, shared with the sweep dedupe.
	recallDiversityThreshold float32 = dedupeCosineThreshold
)

// mmrSelect greedily picks up to k of pts (sorted best-first), skipping any with cosine >= threshold to a
// pick. Not full MMR: it only stops near-duplicates crowding out distinct hits. No Vector = never a dup.
func mmrSelect(pts []scored, k int, threshold float32) []scored {
	if k <= 0 || len(pts) == 0 {
		return nil
	}
	selected := make([]scored, 0, k)
	for _, p := range pts {
		if len(selected) >= k {
			break
		}
		dup := false
		for _, sel := range selected {
			if len(p.Vector) > 0 && len(sel.Vector) > 0 && cosine(p.Vector, sel.Vector) >= threshold {
				dup = true
				break
			}
		}
		if !dup {
			selected = append(selected, p)
		}
	}
	return selected
}

// Store serves one memory collection over a vector index, shared by every agent.
type Store struct {
	idx            index
	embedder       inference.Embedder
	consolidator   model.LLM
	coll           string
	domain         string // selects the consolidation prompt ("task" | "user")
	topK           int
	minScore       float32 // recall hits below this cosine are dropped (0 = none)
	log            *slog.Logger
	embCache       *embedCache
	opsLog         OpsLog             // audit trail sink; nil unless the caller wires one (see SetOpsLog)
	forgetRules    []memoryrules.Rule // nil means memoryrules.DefaultRules() (see SetForgettingRules)
	listErrForTest error              // test-only fault injection, see SetListErrorForTest
}

// SetListErrorForTest makes every forEachSweepPage on this store fail with err until reset,
// for tests outside the package that can't reach the index interface.
func (s *Store) SetListErrorForTest(err error) { s.listErrForTest = err }

// SetOpsLog wires the memory_ops audit sink (internal/memory can't import internal/store).
// nil is a valid silent no-op.
func (s *Store) SetOpsLog(l OpsLog) { s.opsLog = l }

// newStore wraps a backend, probing the embedder for vector dimension.
func newStore(ctx context.Context, idx index, embedder inference.Embedder, consolidator model.LLM, collection, domain string, topK int, minScore float32) (*Store, error) {
	s := &Store{
		idx:          idx,
		embedder:     embedder,
		consolidator: consolidator,
		coll:         collection,
		domain:       domain,
		topK:         topK,
		minScore:     minScore,
		log:          slog.Default().With("component", "memory", "collection", collection),
		embCache:     newEmbedCache(512),
	}
	if err := idx.ensure(ctx, func() (int, error) {
		vecs, err := s.embed(ctx, []string{"dimension probe"}, "dim-probe")
		if err != nil {
			return 0, fmt.Errorf("memory: embed probe: %w", err)
		}
		if len(vecs) == 0 || len(vecs[0]) == 0 {
			return 0, fmt.Errorf("memory: embed probe returned no vector")
		}
		return len(vecs[0]), nil
	}); err != nil {
		return nil, err
	}
	n, err := idx.backfillTiers(ctx)
	if err != nil {
		s.log.Warn("memory tier backfill failed", "err", err)
	} else if n > 0 {
		s.log.Info("memory tier backfill", "touched", n)
	}
	if n, err := idx.backfillJudgeSupport(ctx); err != nil {
		s.log.Warn("memory judge-support backfill failed", "err", err)
	} else if n > 0 {
		s.log.Info("memory judge-support backfill", "touched", n)
	}
	return s, nil
}

// AddSessionToMemory is a deliberate no-op; writes go through the explicit gated commit.
func (s *Store) AddSessionToMemory(ctx context.Context, _ session.Session) error { return nil }

// recall returns top-K memories across buckets plus each entry's scored point in the same order,
// since adkmemory.Entry has no Score field.
func (s *Store) recall(ctx context.Context, buckets []string, query string) (resp *adkmemory.SearchResponse, hits []scored, err error) {
	if len(buckets) == 0 || strings.TrimSpace(query) == "" {
		return &adkmemory.SearchResponse{}, nil, nil
	}
	// Cap the query before embedding - a recall query is a topic, not a document.
	if r := []rune(query); len(r) > maxRecallRunes {
		query = string(r[:maxRecallRunes])
	}
	// Bounded + best-effort: recall must never hang or fail a node.
	ectx, cancel := context.WithTimeout(ctx, recallEmbedTimeout)
	defer cancel()
	vecs, embErr := s.embed(ectx, []string{query}, "recall")
	if embErr != nil {
		s.log.Warn("recall embed failed; proceeding without recall", "err", embErr)
		return &adkmemory.SearchResponse{}, nil, nil
	}
	if len(vecs) == 0 {
		return &adkmemory.SearchResponse{}, nil, nil
	}
	// Apply minScore in Go so the threshold is observable in the returned score.
	pts, qerr := s.idx.query(ctx, buckets, vecs[0], s.topK*recallFetchMultiplier)
	if qerr != nil {
		return nil, nil, fmt.Errorf("memory: query %q: %w", s.coll, qerr)
	}
	entries := make([]adkmemory.Entry, 0, s.topK)
	kept := make([]scored, 0, s.topK)
	previews := make([]string, 0, s.topK)
	var topScore float32
	dropped := 0
	for _, p := range mmrSelect(pts, s.topK, recallDiversityThreshold) {
		if p.Score > topScore {
			topScore = p.Score
		}
		if s.minScore > 0 && p.Score < s.minScore {
			dropped++
			continue
		}
		if p.Content == "" {
			continue
		}
		previews = append(previews, preview(p.Content))
		e := adkmemory.Entry{
			ID:      p.ID,
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: tierPrefix(p.Tier, p.Supported) + p.Content}}},
			Author:  p.Author,
		}
		if p.Timestamp != "" {
			if t, perr := time.Parse(time.RFC3339, p.Timestamp); perr == nil {
				e.Timestamp = t
			}
		}
		entries = append(entries, e)
		kept = append(kept, p)
	}
	s.log.Debug("recall", "buckets", buckets,
		"query", preview(query), "raw", len(pts), "top_score", topScore,
		"min_score", s.minScore, "dropped", dropped, "hits", len(entries), "memories", previews)
	return &adkmemory.SearchResponse{Memories: entries}, kept, nil
}

// DefaultListLimit caps a List/Search that omits limit, so it can't force a full scan.
// Exported so the REST layer's default stays the single source of truth.
const DefaultListLimit = 50

// ErrMemoryNotFound is returned by Forget when id names nothing in the index.
var ErrMemoryNotFound = errors.New("memory: not found")

// ListSort values for Store.List's sortBy; "" means SortNewest.
const (
	SortNewest       = "newest"
	SortOldest       = "oldest"
	SortScore        = "score"         // net vote score (VoteScore), descending
	SortUpvotes      = "upvotes"       // descending
	SortDownvotes    = "downvotes"     // descending
	SortRecalls      = "recalls"       // descending
	SortLastRecalled = "last_recalled" // most recently recalled first; never-recalled last
)

// firstSort picks sortBy[0] if given and non-empty, else SortNewest - the
// shared default for every index.list implementation's variadic arg.
func firstSort(sortBy []string) string {
	if len(sortBy) > 0 && sortBy[0] != "" {
		return sortBy[0]
	}
	return SortNewest
}

// SortMemories orders a merged two-store []Memory by the same ListSort vocabulary index.list applies per store.
func SortMemories(mems []Memory, sortBy string) {
	sort.SliceStable(mems, func(i, j int) bool { return lessBy(sortBy, mems[i].orderKey(), mems[j].orderKey()) })
}

// orderKey holds the fields every ListSort value orders by.
type orderKey struct {
	ID, Timestamp, LastRecalledAt          string
	VoteScore, Upvotes, Downvotes, Recalls int
}

func (m *Memory) orderKey() orderKey {
	return orderKey{m.ID, m.Timestamp, m.LastRecalledAt, m.VoteScore, m.Upvotes, m.Downvotes, m.Recalls}
}

func (p *scored) orderKey() orderKey {
	return orderKey{p.ID, p.Timestamp, p.LastRecalledAt, p.VoteScore, p.Upvotes, p.Downvotes, p.Recalls}
}

// lessBy mirrors sqliteOrderBy: an ID tie-break keeps paging stable, and last_recalled sorts
// "" (never recalled) last, where a bare string compare would put it first.
func lessBy(sortBy string, a, b orderKey) bool {
	if c := compareBy(sortBy, a, b); c != 0 {
		return c < 0
	}
	return a.ID > b.ID
}

func compareBy(sortBy string, a, b orderKey) int {
	switch sortBy {
	case SortOldest:
		return cmp.Compare(a.Timestamp, b.Timestamp)
	case SortScore:
		return cmp.Compare(b.VoteScore, a.VoteScore)
	case SortUpvotes:
		return cmp.Compare(b.Upvotes, a.Upvotes)
	case SortDownvotes:
		return cmp.Compare(b.Downvotes, a.Downvotes)
	case SortRecalls:
		return cmp.Compare(b.Recalls, a.Recalls)
	case SortLastRecalled:
		if a.LastRecalledAt == "" && b.LastRecalledAt != "" {
			return 1
		}
		if b.LastRecalledAt == "" && a.LastRecalledAt != "" {
			return -1
		}
		return cmp.Compare(b.LastRecalledAt, a.LastRecalledAt)
	default: // SortNewest
		return cmp.Compare(b.Timestamp, a.Timestamp)
	}
}

// Memory is one entry as the explorer sees it. Score is set only by Search.
type Memory struct {
	ID        string
	Content   string
	Bucket    string
	Author    string
	Timestamp string
	Kind      string
	ChatID    string // provenance: minting chat, used by `quack memory rescope` to find its origin
	Score     float32

	// Status "" reads as unverified (pre-lifecycle point).
	Status             string
	ReinforcementCount int
	InvalidationReason string

	// Supported/NotRelevant: see scored's field doc.
	Upvotes        int
	Downvotes      int
	Supported      int
	NotRelevant    int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string

	// AbsorbedIDs: see scored.AbsorbedIDs.
	AbsorbedIDs []string
	HumanVote   string // "up" | "down" | ""
}

// List returns one page (limit<=0 = DefaultListLimit) plus the total matching the same filters, filtered
// index-side so they span pages. Unlike recall, an index failure is returned, never degraded.
func (s *Store) List(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool, tier string, sortBy ...string) ([]Memory, int, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if offset < 0 {
		offset = 0
	}
	pts, err := s.idx.list(ctx, buckets, offset, limit, includeInvalidated, tier, false, sortBy...)
	if err != nil {
		return nil, 0, fmt.Errorf("memory: list %q: %w", s.coll, err)
	}
	total, err := s.idx.count(ctx, buckets, includeInvalidated, tier)
	if err != nil {
		return nil, 0, fmt.Errorf("memory: count %q: %w", s.coll, err)
	}
	return toMemories(pts), total, nil
}

// GetByID fetches one memory by id, invalidated or not (callers check Status).
// ErrMemoryNotFound if this store doesn't have it.
func (s *Store) GetByID(ctx context.Context, id string) (Memory, error) {
	pt, ok, err := s.idx.getByID(ctx, id)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: get %q: %w", id, err)
	}
	if !ok {
		return Memory{}, ErrMemoryNotFound
	}
	return toMemories([]scored{pt})[0], nil
}

// Search returns up to limit memories ranked by cosine, descending. Unlike recall,
// an embed or index failure is returned, not swallowed.
func (s *Store) Search(ctx context.Context, buckets []string, q string, limit int) ([]Memory, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("memory: search: empty query")
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	vecs, err := s.embed(ctx, []string{q}, "explorer-search")
	if err != nil {
		return nil, fmt.Errorf("memory: embed: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("memory: embed returned no vector")
	}
	pts, err := s.idx.query(ctx, buckets, vecs[0], limit)
	if err != nil {
		return nil, fmt.Errorf("memory: query %q: %w", s.coll, err)
	}
	return toMemories(pts), nil
}

// Forget deletes one memory by id - a real delete against the index, not a
// tombstone. ErrMemoryNotFound if id isn't in the index.
func (s *Store) Forget(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrMemoryNotFound
	}
	n, err := s.idx.remove(ctx, []string{id})
	if err != nil {
		return fmt.Errorf("memory: forget %q: %w", id, err)
	}
	if n == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// InvalidateByID soft-invalidates one memory (the human-delete path) and writes one memory_ops row.
// ErrMemoryNotFound if absent; re-invalidating is idempotent.
func (s *Store) InvalidateByID(ctx context.Context, id, reason string, actor OpsLogActor) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrMemoryNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual delete"
	}
	n, err := s.idx.invalidateByID(ctx, []string{id}, reason)
	if err != nil {
		return fmt.Errorf("memory: invalidate %q: %w", id, err)
	}
	if n == 0 {
		return ErrMemoryNotFound
	}
	s.logOp(ctx, id, OpInvalidate, actor, reason)
	return nil
}

func toMemories(pts []scored) []Memory {
	out := make([]Memory, len(pts))
	for i, p := range pts {
		out[i] = Memory{
			ID: p.ID, Content: p.Content, Bucket: p.Scope, Author: p.Author, Timestamp: p.Timestamp, Kind: p.Kind, ChatID: p.ChatID, Score: p.Score,
			Status: p.Status, ReinforcementCount: p.ReinforcementCount, InvalidationReason: p.InvalidationReason,
			Upvotes: p.Upvotes, Downvotes: p.Downvotes, Supported: p.Supported, NotRelevant: p.NotRelevant, VoteScore: p.VoteScore, Tier: p.Tier,
			LastUpvotedAt: p.LastUpvotedAt, Recalls: p.Recalls, LastRecalledAt: p.LastRecalledAt,
			AbsorbedIDs: p.AbsorbedIDs, HumanVote: p.HumanVote,
		}
	}
	return out
}

// embed wraps the embedder with hot-path timing.
func (s *Store) embed(ctx context.Context, texts []string, path string) ([][]float32, error) {
	// Single-input calls are memoized; batch writes are not worth caching.
	if len(texts) == 1 && s.embCache != nil {
		if v, ok := s.embCache.get(texts[0]); ok {
			s.log.Debug("embed", "path", path, "inputs", 1, "chars", len(texts[0]), "cached", true)
			return [][]float32{v}, nil
		}
	}
	chars := 0
	for _, t := range texts {
		chars += len(t)
	}
	t0 := time.Now()
	vecs, err := s.embedder.Embed(ctx, texts)
	s.log.Debug("embed", "path", path, "inputs", len(texts), "chars", chars, "dur", time.Since(t0), "err", err != nil)
	if err == nil && len(texts) == 1 && len(vecs) == 1 && s.embCache != nil {
		s.embCache.put(texts[0], vecs[0])
	}
	return vecs, err
}

// embedCache memoizes text→embedding. ponytail: clear-on-full, not LRU.
type embedCache struct {
	mu  sync.Mutex
	m   map[string][]float32
	cap int
}

func newEmbedCache(cap int) *embedCache {
	return &embedCache{m: make(map[string][]float32, cap), cap: cap}
}

func (c *embedCache) get(k string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *embedCache) put(k string, v []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.cap {
		c.m = make(map[string][]float32, c.cap)
	}
	c.m[k] = v
}

// preview truncates a string to ~100 runes for debug logging.
func preview(s string) string {
	const max = 100
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
