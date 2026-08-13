// Package memory is Quack's semantic-memory layer (M6) over a swappable vector index.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	list(ctx context.Context, buckets []string, offset, limit int) ([]scored, error)
	// count returns how many points match buckets (all buckets if empty).
	count(ctx context.Context, buckets []string) (int, error)
	upsert(ctx context.Context, pts []point) error
	// remove deletes the named ids and reports how many actually existed.
	remove(ctx context.Context, ids []string) (int, error)
	// invalidateByID soft-invalidates the named ids in place (status=invalidated,
	// invalidated_at=now, invalidation_reason=reason) - the consolidator's DELETE,
	// which never removes a point (design doc §4(a)/§8 phase 2, soft-delete only).
	invalidateByID(ctx context.Context, ids []string, reason string) error
	// updateStatus applies an outcome to every point matching chatID that is not
	// already invalidated (sticky: nothing revives an invalidated memory), and
	// returns the ids actually touched - a payload-only mutation, no re-embed.
	updateStatus(ctx context.Context, chatID string, o OutcomeSignal) ([]string, error)
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
}

const (
	// maxRecallRunes bounds recall query size so one oversized input can't stall the embedder.
	maxRecallRunes = 2000
	// recallEmbedTimeout bounds how long recall waits before degrading to no-recall.
	recallEmbedTimeout = 30 * time.Second
)

// Store serves one memory collection over a vector index, shared by every agent.
type Store struct {
	idx          index
	embedder     inference.Embedder
	consolidator model.LLM
	coll         string
	domain       string // selects the consolidation prompt ("task" | "user")
	topK         int
	minScore     float32 // recall hits below this cosine are dropped (0 = none)
	log          *slog.Logger
	embCache     *embedCache
	opsLog       OpsLog // audit trail sink; nil unless the caller wires one (see SetOpsLog)
}

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
	return s, nil
}

// AddSessionToMemory is a deliberate no-op; writes go through the explicit gated commit.
func (s *Store) AddSessionToMemory(ctx context.Context, _ session.Session) error { return nil }

// recall embeds the query and returns top-K memories across the caller's buckets.
func (s *Store) recall(ctx context.Context, buckets []string, query string) (*adkmemory.SearchResponse, error) {
	if len(buckets) == 0 || strings.TrimSpace(query) == "" {
		return &adkmemory.SearchResponse{}, nil
	}
	// Cap the query before embedding - a recall query is a topic, not a document.
	if r := []rune(query); len(r) > maxRecallRunes {
		query = string(r[:maxRecallRunes])
	}
	// Bounded + best-effort: recall must never hang or fail a node.
	ectx, cancel := context.WithTimeout(ctx, recallEmbedTimeout)
	defer cancel()
	vecs, err := s.embed(ectx, []string{query}, "recall")
	if err != nil {
		s.log.Warn("recall embed failed; proceeding without recall", "err", err)
		return &adkmemory.SearchResponse{}, nil
	}
	if len(vecs) == 0 {
		return &adkmemory.SearchResponse{}, nil
	}
	// Fetch top-K then apply minScore in Go so the threshold is observable.
	pts, err := s.idx.query(ctx, buckets, vecs[0], s.topK)
	if err != nil {
		return nil, fmt.Errorf("memory: query %q: %w", s.coll, err)
	}
	entries := make([]adkmemory.Entry, 0, len(pts))
	previews := make([]string, 0, len(pts))
	var topScore float32
	dropped := 0
	for _, p := range pts {
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
	}
	// Debug log: buckets, raw matches, top_score, dropped, hits.
	s.log.Debug("recall", "buckets", buckets,
		"query", preview(query), "raw", len(pts), "top_score", topScore,
		"min_score", s.minScore, "dropped", dropped, "hits", len(entries), "memories", previews)
	return &adkmemory.SearchResponse{Memories: entries}, nil
}

// DefaultListLimit caps an unbounded List/Search request so one caller can't
// force a full-collection scan by omitting limit. Exported so the REST layer's
// own default (the `limit` query param) stays the single source of truth.
const DefaultListLimit = 50

// ErrMemoryNotFound is returned by Forget when id names nothing in the index.
var ErrMemoryNotFound = errors.New("memory: not found")

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
	Score     float32
}

// List returns entries in the given buckets (every bucket if empty), newest
// first, paged by offset/limit (limit<=0 defaults to DefaultListLimit), plus
// the total count matching the same filter. Unlike Search/recall, this never
// falls back to embedding search and never degrades on a failure - an
// unreachable index is returned as an error, not an empty or partial result.
func (s *Store) List(ctx context.Context, buckets []string, offset, limit int) ([]Memory, int, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if offset < 0 {
		offset = 0
	}
	pts, err := s.idx.list(ctx, buckets, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("memory: list %q: %w", s.coll, err)
	}
	total, err := s.idx.count(ctx, buckets)
	if err != nil {
		return nil, 0, fmt.Errorf("memory: count %q: %w", s.coll, err)
	}
	return toMemories(pts), total, nil
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

func toMemories(pts []scored) []Memory {
	out := make([]Memory, len(pts))
	for i, p := range pts {
		out[i] = Memory{ID: p.ID, Content: p.Content, Bucket: p.Scope, Author: p.Author, Timestamp: p.Timestamp, Kind: p.Kind, Score: p.Score}
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
