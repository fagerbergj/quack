// Package memory is Quack's semantic-memory layer (M6): a Qdrant-backed
// implementation of ADK's memory.Service. Recall (SearchMemory) is live here;
// ADK's native preload_memory / load_memory tools route through it via the
// runner's MemoryService. Whole-session auto-write (AddSessionToMemory) is a
// deliberate no-op — writes go through the explicit, gated commit path (later
// PRs), never ADK's automatic ingestion.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"
	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
)

// Payload keys stored on each memory point. SearchMemory reads them; the commit
// path (later PR) writes them.
const (
	payloadContent   = "content"
	payloadUserID    = "user_id"
	payloadAuthor    = "author"
	payloadTimestamp = "timestamp"
)

// Store is a Qdrant collection serving one memory scope (e.g. "task_memory").
// It implements adkmemory.Service.
type Store struct {
	client   *qdrant.Client
	embedder inference.Embedder
	coll     string
	topK     uint64
	log      *slog.Logger
}

var _ adkmemory.Service = (*Store)(nil)

// Open connects to Qdrant at addr (host:port gRPC; scheme and default port 6334
// are tolerated), and ensures the scope's collection exists — creating it on
// first use with a vector size probed from the embedder, so the embedding model's
// dimension need not be configured.
func Open(ctx context.Context, addr string, embedder inference.Embedder, collection string, topK int) (*Store, error) {
	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	client, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		return nil, fmt.Errorf("memory: qdrant client: %w", err)
	}
	s := &Store{
		client:   client,
		embedder: embedder,
		coll:     collection,
		topK:     uint64(topK),
		log:      slog.Default().With("component", "memory", "collection", collection),
	}
	if err := s.ensureCollection(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureCollection(ctx context.Context) error {
	exists, err := s.client.CollectionExists(ctx, s.coll)
	if err != nil {
		return fmt.Errorf("memory: collection exists %q: %w", s.coll, err)
	}
	if exists {
		return nil
	}
	// Probe the embedder for the vector dimension rather than hardcoding the model's.
	vecs, err := s.embedder.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return fmt.Errorf("memory: embed probe: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return fmt.Errorf("memory: embed probe returned no vector")
	}
	dim := uint64(len(vecs[0]))
	if err := s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: s.coll,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: dim, Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		return fmt.Errorf("memory: create collection %q: %w", s.coll, err)
	}
	s.log.Info("created qdrant collection", "dim", dim)
	return nil
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
	vecs, err := s.embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return &adkmemory.SearchResponse{}, nil
	}
	var flt *qdrant.Filter
	if req.UserID != "" {
		flt = &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword(payloadUserID, req.UserID)}}
	}
	pts, err := s.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: s.coll,
		Query:          qdrant.NewQueryDense(vecs[0]),
		Limit:          &s.topK,
		Filter:         flt,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("memory: query %q: %w", s.coll, err)
	}
	entries := make([]adkmemory.Entry, 0, len(pts))
	for _, p := range pts {
		content := payloadString(p.GetPayload(), payloadContent)
		if content == "" {
			continue
		}
		e := adkmemory.Entry{
			ID:      pointID(p.GetId()),
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: content}}},
			Author:  payloadString(p.GetPayload(), payloadAuthor),
		}
		if ts := payloadString(p.GetPayload(), payloadTimestamp); ts != "" {
			if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
				e.Timestamp = t
			}
		}
		entries = append(entries, e)
	}
	s.log.Debug("recall", "user", req.UserID, "hits", len(entries))
	return &adkmemory.SearchResponse{Memories: entries}, nil
}

func payloadString(payload map[string]*qdrant.Value, key string) string {
	if v, ok := payload[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

func pointID(id *qdrant.PointId) string {
	if id == nil {
		return ""
	}
	if u := id.GetUuid(); u != "" {
		return u
	}
	return strconv.FormatUint(id.GetNum(), 10)
}

// parseAddr splits a Qdrant gRPC address (host:port) into host + port.
func parseAddr(raw string) (string, int, error) {
	host, p, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("memory: QDRANT_URL must be host:port: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("memory: bad port %q: %w", p, err)
	}
	return host, port, nil
}
