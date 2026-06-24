// Package memory is Quack's semantic-memory layer (M6): an implementation of
// ADK's memory.Service over a swappable vector index (Qdrant for a server, or an
// embedded SQLite file for the no-docker path). All the embed / scope / recall /
// mem0-style consolidation logic lives on Store; the backend is just storage,
// behind the index interface. Recall (SearchMemory) is live here; ADK's native
// preload_memory / load_memory tools route through it via the runner's
// MemoryService. Whole-session auto-write (AddSessionToMemory) is a deliberate
// no-op — writes go through the explicit, gated commit path, never ADK's
// automatic ingestion.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
)

// index is the vector-storage backend behind a Store. Qdrant and SQLite implement
// it; Store holds the shared embed/scope/consolidation logic, so a backend is just
// storage. Points are partitioned by a scope key (agent name or user id), set on
// upsert and filtered on query.
type index interface {
	// ensure makes the backing collection/table ready. probeDim returns the
	// embedding dimension, called only by a backend that needs a fixed vector size
	// (Qdrant); SQLite stores variable-length blobs and ignores it.
	ensure(ctx context.Context, probeDim func() (int, error)) error
	// query returns up to k points whose scope matches, nearest to vec by cosine,
	// best score first. An empty scope means no partition filter.
	query(ctx context.Context, scope string, vec []float32, k int) ([]scored, error)
	upsert(ctx context.Context, pts []point) error
	remove(ctx context.Context, ids []string) error
}

// scored is one ranked memory returned by index.query.
type scored struct {
	ID        string
	Content   string
	Author    string
	Timestamp string
	Score     float32
}

// point is one memory to upsert. Scope is the partition key (stored so query can
// filter by it).
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
	// maxRecallRunes bounds the recall query sent to the embedder (a topic, not a
	// document) so one oversized input can't become a minutes-long CPU embed.
	maxRecallRunes = 2000
	// recallEmbedTimeout bounds how long recall waits on the embedder before
	// degrading to no-recall — recall is best-effort and must not block a node.
	recallEmbedTimeout = 30 * time.Second
)

// Store serves one memory scope (e.g. "task_memory") over a vector index. It
// implements adkmemory.Service. The consolidator (a gemma-class LLM) drives the
// gated commit path's extract/vet/consolidate step; it may be nil for a read-only
// store (Commit then errors).
type Store struct {
	idx          index
	embedder     inference.Embedder
	consolidator model.LLM
	coll         string
	domain       string // selects the consolidation prompt ("task" | "user")
	topK         int
	minScore     float32 // recall hits below this cosine score are dropped (0 = no threshold)
	log          *slog.Logger
	embCache     *embedCache // memoizes text→vector (deterministic for a fixed model)
}

var _ adkmemory.Service = (*Store)(nil)

// newStore wraps a backend index with the shared memory logic and ensures the
// backing store is ready (probing the embedder for the vector dimension on first
// use, so the model's dimension need not be configured).
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

// AddSessionToMemory is a deliberate no-op (implements adkmemory.Service): Quack
// never auto-ingests whole sessions; writes go through the explicit gated commit.
func (s *Store) AddSessionToMemory(ctx context.Context, _ session.Session) error { return nil }

// SearchMemory embeds the query and returns the top-K nearest memories in this
// scope, filtered to the requesting user. Implements adkmemory.Service.
func (s *Store) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if req == nil || strings.TrimSpace(req.Query) == "" {
		return &adkmemory.SearchResponse{}, nil
	}
	// Cap the query before embedding: a recall query is a topic, not a document.
	// Without this an agent whose input is huge (e.g. a combiner fed all upstream
	// findings) would embed tens of KB on the CPU embedder — a 10-min job that
	// head-of-line-blocks llama.cpp and stalls the DAG.
	query := req.Query
	if r := []rune(query); len(r) > maxRecallRunes {
		query = string(r[:maxRecallRunes])
	}
	// Bounded + best-effort: recall must never hang or fail a node. A slow/wedged
	// embedder times out here and we proceed with no recall rather than blocking.
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
	scopeID := s.scope(req.AppName, req.UserID)
	// Fetch top-K by similarity within the scope, then apply minScore in Go so the
	// threshold decision is observable (raw match count + top score in the debug log).
	// Dropping weak matches keeps low-relevance hits out of the prompt as the
	// collection grows (context rot).
	pts, err := s.idx.query(ctx, scopeID, vecs[0], s.topK)
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
	// Logs what preload_memory / load_memory pulled in (both route here). Enable with
	// LOG_LEVEL=debug. scope = the partition key actually queried (agent name for task,
	// userID for user); raw = matches in scope before minScore; top_score = best cosine;
	// dropped = filtered by minScore. This pinpoints hits=0: scope mismatch (raw=0) vs
	// threshold too high (raw>0, dropped=raw).
	s.log.Debug("recall", "user", req.UserID, "scope", scopeID, "app", req.AppName,
		"query", preview(req.Query), "raw", len(pts), "top_score", topScore,
		"min_score", s.minScore, "dropped", dropped, "hits", len(entries), "memories", previews)
	return &adkmemory.SearchResponse{Memories: entries}, nil
}

// embed wraps the embedder with hot-path timing (Debug): which call site (path),
// how many inputs, total input chars, and how long the call took — so a slow
// embed in the llm-swap logs can be attributed to recall vs commit and to input
// size. Enable with LOG_LEVEL=debug.
func (s *Store) embed(ctx context.Context, texts []string, path string) ([][]float32, error) {
	// Single-input calls (recall, neighbour probe) are memoized: preload_memory
	// re-embeds the same node-task query on every model turn, and that embed is
	// synchronous + CPU-bound, so caching collapses the repeats (and the resulting
	// cross-node contention). Batch writes are unique facts — not worth caching.
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

// embedCache memoizes text→embedding. Embeddings are deterministic for a fixed
// model (stable for the process lifetime), so no TTL is needed; it's bounded by a
// size cap. ponytail: clear-on-full, not LRU — distinct queries per request are
// few, so a 512-entry cap rarely fills; switch to an LRU only if real churn shows.
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

// preview truncates a string to ~100 runes for debug logging (memories are short
// facts, but a stray long one shouldn't flood the log).
func preview(s string) string {
	const max = 100
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// scope returns the partition key for this store's domain. Task memory is
// per-agent *tradecraft*, so it is keyed by the agent name (recall: req.AppName;
// commit: the author) — stable across requests, unlike the per-invocation
// "A2A_USER_<ctxid>" the A2A server mints. User memory is personal, so it stays
// keyed by the real userID.
func (s *Store) scope(agentName, userID string) string {
	if s.domain == "task" {
		return agentName
	}
	return userID
}
