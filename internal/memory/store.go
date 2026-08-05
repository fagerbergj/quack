// Package memory is Quack's semantic-memory layer (M6) over a swappable vector index.
package memory

import (
	"context"
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
	upsert(ctx context.Context, pts []point) error
	remove(ctx context.Context, ids []string) error
}

// scored is one ranked memory.
type scored struct {
	ID        string
	Content   string
	Author    string
	Timestamp string
	Score     float32
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
}

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
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: p.Content}}},
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
