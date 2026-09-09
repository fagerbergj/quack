// Package memory is Quack's semantic-memory layer (M6) over a swappable vector index.
package memory

import (
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
	// list returns up to `limit` points in any of the given buckets (all buckets if
	// empty), newest first by Timestamp, skipping `offset`. limit<=0 means no cap.
	// includeInvalidated=false excludes status=invalidated points, matching the
	// query()/recall filter (design doc §4(d) extended to the browse surface, phase 3).
	// tier=="" means no tier filter; "unverified" also matches a point that
	// predates the tier field (empty/missing tier reads as unverified
	// everywhere else in this package - review finding 10). withVectors
	// populates each result's Vector from the already-stored embedding
	// (DedupeSweep's clustering, ) - never a re-embed, both
	// backends already have it on hand at list time; false everywhere else to
	// skip the extra payload. sortBy is variadic so every existing
	// caller's positional call keeps compiling unchanged past this second
	// added parameter: sortBy[0], if given and non-empty, is one of the
	// ListSort constants below and orders the WHOLE matching set (index-side
	// for sqlite, in-Go for qdrant which already fetches everything) before
	// offset/limit slice it, so a sort spans pages correctly.
	list(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool, tier string, withVectors bool, sortBy ...string) ([]scored, error)
	// scrollAll walks every point across ALL buckets in pages of pageSize,
	// calling fn once per page until exhausted, visiting each point exactly
	// once - the sweep jobs' access pattern, which doesn't need sorted order.
	// Unlike calling list() in an offset loop, an implementation can thread a
	// single native cursor across the whole walk (see qdrantIndex.scrollAll).
	scrollAll(ctx context.Context, includeInvalidated, withVectors bool, pageSize int, fn func([]scored)) error
	// count returns how many points match buckets (all buckets if empty), under the
	// same includeInvalidated/tier filter as list.
	count(ctx context.Context, buckets []string, includeInvalidated bool, tier string) (int, error)
	upsert(ctx context.Context, pts []point) error
	// getByID fetches one point by id, including an invalidated one (the
	// caller decides what to do with that) - a direct lookup, not a
	// list-and-scan, so it's correct past whatever page size list()/List()
	// cap at. ok=false if id doesn't exist in this collection.
	getByID(ctx context.Context, id string) (pt scored, ok bool, err error)
	// remove deletes the named ids and reports how many actually existed.
	remove(ctx context.Context, ids []string) (int, error)
	// invalidateByID soft-invalidates the named ids in place (status=invalidated,
	// invalidated_at=now, invalidation_reason=reason) - the consolidator's DELETE
	// and the human-delete REST path, neither of which ever removes a point
	// (design doc §4(a)/(b), soft-delete only). Reports how many ids actually existed.
	invalidateByID(ctx context.Context, ids []string, reason string) (int, error)
	// updateStatus applies an outcome to every id in ids that is not already
	// invalidated (sticky) and, for OutcomeInvalidated, not already tier
	// "verified" (design decision #1255: a verified memory recalled into a
	// closed-unmerged chat gets no vote, not an invalidation). Returns the
	// ids actually touched - a payload-only mutation, no re-embed.
	updateStatus(ctx context.Context, ids []string, o OutcomeSignal) ([]string, error)
	// applyVotes applies each vote to its memory id (skipping an
	// already-invalidated one, sticky) and returns the ids touched. A
	// net score <= invalidateThreshold invalidates the memory (reason
	// OutcomeReasonNetScore), same soft-invalidate as everything else.
	applyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) ([]string, error)
	// recordRecall bumps recalls and stamps last_recalled_at for ids, one
	// batched write - the usage-tracking half of a recall delivery.
	recordRecall(ctx context.Context, ids []string) error
	// backfillTiers is the one-time migration (epic P1) for every
	// point with no tier yet: verified (upvotes=reinforcement_count) if
	// reinforcement_count >= 1, else unverified. Idempotent - a point that
	// already carries a tier is left alone, so a second boot touches none.
	backfillTiers(ctx context.Context) (int, error)
	// updateBucket moves a single point to a new bucket key (#1262's
	// `quack memory rescope`) - a payload/column-only mutation, no re-embed.
	updateBucket(ctx context.Context, id, bucket string) error
	// absorb folds absorbedID's votes/timestamps/lineage into survivorID
	// (epic P5 consolidation merge) and invalidates absorbedID with
	// reason. Returns false (no-op) if either id doesn't exist, or absorbedID
	// is already invalidated (sticky - the first invalidation wins).
	absorb(ctx context.Context, survivorID, absorbedID, reason string) (bool, error)
	// setHumanVote sets the caller's own vote ("up"/"down"/"none") on id,
	// re-deriving upvotes/downvotes/vote_score/tier from the transition away
	// from the point's PRIOR human_vote (so a toggle or a flip never double
	// counts). Reports whether id existed (and wasn't already invalidated).
	setHumanVote(ctx context.Context, id, vote string, invalidateThreshold int) (bool, error)
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
	// Lifecycle (design doc §3/§4, phase 2): empty Status means the point predates
	// this phase and reads as StatusUnverified everywhere (recall filter, tier prefix).
	Status             string
	ValidFrom          string
	InvalidatedAt      string
	InvalidationReason string
	ReinforcementCount int
	Score              float32

	// Vote fields (epic P1): Upvotes/Downvotes/VoteScore are the
	// judge's (or a human's) accumulated votes on this memory, independent
	// of Score (cosine rank). Tier is "verified" once Upvotes >= 1, else
	// "unverified" - never demoted by a downvote alone (only net<=-2
	// invalidates, via Status).
	Upvotes        int
	Downvotes      int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string

	// AbsorbedIDs (epic P5): ids of memories consolidation merged into
	// this one (near-duplicate merge or supersession), flattened across any
	// absorption chain - see internal/memory/lineage.go.
	AbsorbedIDs []string
	// HumanVote is the single-user deployment's own current vote ("up"/"down",
	// "" = none) - epic P4, distinct from Upvotes/Downvotes which mix
	// judge and human votes together. Toggling re-derives the delta from this.
	HumanVote string
	// Vector is populated by query() only (list()/getByID leave it nil) - the
	// MMR diversity re-rank in recall ( ) needs each hit's own
	// embedding to compute inter-hit cosine, which the query score alone
	// (similarity to the QUERY, not to other hits) can't give it.
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
	// recallFetchMultiplier: recall fetches this many times topK from the index
	// so mmrSelect ( ) has enough candidates to pick a diverse top-K
	// from, instead of only ever seeing exactly topK (no room to swap a
	// near-duplicate for the next-best distinct hit).
	recallFetchMultiplier = 2
	// recallDiversityThreshold: mmrSelect drops a candidate whose cosine to an
	// already-selected hit is at or above this - the same near-duplicate bar
	// the sweep dedupe uses (dedupeCosineThreshold).
	recallDiversityThreshold float32 = dedupeCosineThreshold
)

// mmrSelect greedily picks up to k of pts (already sorted best-first by the
// index) such that no two selected points are mutual near-duplicates: a
// candidate is skipped if its cosine similarity to any already-selected
// point is >= threshold. This is deliberately not full MMR (no relevance/
// diversity tradeoff parameter) - the goal is only to stop near-identical
// hits from crowding out a distinct one, not to optimize diversity for its
// own sake. A point with no Vector (e.g. an index/test double that never
// populates it) can never be judged a duplicate of anything and is kept.
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
	opsLog         OpsLog // audit trail sink; nil unless the caller wires one (see SetOpsLog)
	forgetRules    []Rule // epic P3; nil means DefaultRules() (see SetForgettingRules)
	listErrForTest error  // test-only fault injection, see SetListErrorForTest
}

// SetListErrorForTest forces every forEachSweepPage (and so ForgetSweep) call
// on this store to fail with err (sticky - persists until reset), without
// touching the real index - used by REST/CLI tests one layer up that can't
// reach the unexported index interface to simulate a later store's list-phase
// failure.
func (s *Store) SetListErrorForTest(err error) { s.listErrForTest = err }

// SetOpsLog wires the memory_ops audit sink. internal/memory can't import
// internal/store (dependency direction runs the other way) - the server
// bootstrap (internal/serve) constructs a store-backed OpsLog and calls this
// after opening the Store. Unwired (nil) is a valid, silent no-op - tests and
// a recall-only Store don't need an audit trail.
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
	return s, nil
}

// AddSessionToMemory is a deliberate no-op; writes go through the explicit gated commit.
func (s *Store) AddSessionToMemory(ctx context.Context, _ session.Session) error { return nil }

// recall embeds the query and returns top-K memories across the caller's
// buckets, plus the backing scored point for each returned entry, SAME
// ORDER and length as resp.Memories - adkmemory.Entry (ADK's own type) has
// no Score field, so a caller that needs the cosine score (RecallWithHits,
// for the ledger's memory.recall entry) can't get it from resp alone.
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
	// Fetch 2*topK so the MMR re-rank below has enough candidates to pick a
	// diverse top-K from, then apply minScore in Go so the threshold is observable.
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
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: tierPrefix(p.Status, p.ReinforcementCount) + p.Content}}},
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
	// Debug log: buckets, raw matches, top_score, dropped, hits.
	s.log.Debug("recall", "buckets", buckets,
		"query", preview(query), "raw", len(pts), "top_score", topScore,
		"min_score", s.minScore, "dropped", dropped, "hits", len(entries), "memories", previews)
	return &adkmemory.SearchResponse{Memories: entries}, kept, nil
}

// DefaultListLimit caps an unbounded List/Search request so one caller can't
// force a full-collection scan by omitting limit. Exported so the REST layer's
// own default (the `limit` query param) stays the single source of truth.
const DefaultListLimit = 50

// ErrMemoryNotFound is returned by Forget when id names nothing in the index.
var ErrMemoryNotFound = errors.New("memory: not found")

// ListSort values for Store.List/index.list's variadic sortBy (#1266, owner
// follow-up to the mobile layout fix). "" (or omitted) means SortNewest.
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

// SortMemories orders a merged, in-Go []Memory (the REST handler's two-store
// merge path, #1266) by the same ListSort vocabulary each index.list applies
// server-side for a single store - so a deployment with both task and user
// memory enabled sorts identically to one with either alone, not just by
// timestamp regardless of the requested sort.
func SortMemories(mems []Memory, sortBy string) {
	less := func(i, j int) bool {
		switch sortBy {
		case SortOldest:
			if mems[i].Timestamp != mems[j].Timestamp {
				return mems[i].Timestamp < mems[j].Timestamp
			}
		case SortScore:
			if mems[i].VoteScore != mems[j].VoteScore {
				return mems[i].VoteScore > mems[j].VoteScore
			}
		case SortUpvotes:
			if mems[i].Upvotes != mems[j].Upvotes {
				return mems[i].Upvotes > mems[j].Upvotes
			}
		case SortDownvotes:
			if mems[i].Downvotes != mems[j].Downvotes {
				return mems[i].Downvotes > mems[j].Downvotes
			}
		case SortRecalls:
			if mems[i].Recalls != mems[j].Recalls {
				return mems[i].Recalls > mems[j].Recalls
			}
		case SortLastRecalled:
			iEmpty, jEmpty := mems[i].LastRecalledAt == "", mems[j].LastRecalledAt == ""
			if iEmpty != jEmpty {
				return jEmpty
			}
			if mems[i].LastRecalledAt != mems[j].LastRecalledAt {
				return mems[i].LastRecalledAt > mems[j].LastRecalledAt
			}
		default: // SortNewest
			if mems[i].Timestamp != mems[j].Timestamp {
				return mems[i].Timestamp > mems[j].Timestamp
			}
		}
		return mems[i].ID > mems[j].ID // tie-break, every sort
	}
	sort.SliceStable(mems, less)
}

// Memory is one entry as the explorer (browse or search) sees it - the M6
// storage-layer scored/point pair flattened to what a caller outside this
// package needs. Score is meaningful only from Search; List leaves it zero.
type Memory struct {
	ID        string
	Content   string
	Bucket    string
	Author    string
	Timestamp string
	Kind      string
	ChatID    string // provenance: minting chat, used by `quack memory rescope` to find its origin
	Score     float32

	// Lifecycle (design doc §3). Status "" reads as unverified (pre-lifecycle point).
	Status             string
	ReinforcementCount int
	InvalidationReason string

	// Vote fields (epic P1).
	Upvotes        int
	Downvotes      int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string

	// AbsorbedIDs: see scored.AbsorbedIDs.
	AbsorbedIDs []string
	HumanVote   string // "up" | "down" | "" (epic P4)
}

// List returns entries in the given buckets (every bucket if empty), newest
// first, paged by offset/limit (limit<=0 defaults to DefaultListLimit), plus
// the total count matching the same filter. includeInvalidated=false (the
// default listing) excludes status=invalidated entries from both the page and
// the total, matching query()/recall's backend-level filter (design doc §4(d)).
// tier=="" means no tier filter; "unverified"/"verified" filter server-side
// (index-level, not a post-fetch Go filter) so it spans pages correctly
// instead of only ever seeing whatever's on the
// current page. Unlike Search/recall, this never falls back to embedding
// search and never degrades on a failure - an unreachable index is returned
// as an error, not an empty or partial result.
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

// GetByID fetches one memory directly by id (including an invalidated one -
// callers that care about status check Memory.Status themselves), not by
// paging through List - correct regardless of how many memories exist or
// which page id would land on. ErrMemoryNotFound if this store doesn't have it.
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

// Search embeds q and returns up to `limit` memories across buckets ranked by
// cosine score, descending - "what would a run recall for this". Unlike the
// ADK-facing recall path, an embed or index failure is returned, not swallowed.
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

// InvalidateByID soft-invalidates one memory by id directly - the human-delete
// REST path (design doc §4(b)), which targets a specific point rather than
// matching by provenance chat_id like ApplyOutcome. Writes one memory_ops row
// (op=invalidate) under the given actor. ErrMemoryNotFound if id isn't in the
// index (invalidating an already-invalidated id is idempotent, not an error).
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
			Upvotes: p.Upvotes, Downvotes: p.Downvotes, VoteScore: p.VoteScore, Tier: p.Tier,
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
