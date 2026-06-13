package memory

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// Qdrant payload field names. Kept in one place because both the writer
// (Commit) and the reader (SearchMemory + the search filter) must agree.
const (
	fieldAppName   = "app_name"
	fieldUserID    = "user_id"
	fieldFinding   = "finding"
	fieldQuery     = "query"
	fieldAgent     = "agent"
	fieldSources   = "sources"
	fieldScore     = "score"
	fieldTimestamp = "timestamp"
)

// searchLimit caps how many memories a recall returns. Small: recall is a hint
// to the model, not a context dump.
const searchLimit = 5

// Service is a Qdrant-backed implementation of the ADK memory.Service interface
// plus a Commit helper for the trust gate. The ADK runner calls SearchMemory
// (via the load_memory / preload_memory tools); the gate calls Commit after a
// passing judge verdict.
type Service struct {
	embedder Embedder
	qdrant   *qdrantClient
}

// Committer is the narrow write side of memory the trust gate depends on, so the
// gate package needn't import the whole Service.
type Committer interface {
	Commit(ctx context.Context, req CommitRequest) error
}

// CommitRequest is one vetted finding to store. AppName + UserID define the
// namespace (and must match what the recall adapter searches under — the
// agent's own app name and the request's user). Query is the request that
// produced the finding; it doubles as the dedup key.
type CommitRequest struct {
	AppName string
	UserID  string
	Agent   string
	Query   string
	Finding string
	Sources []string
	Score   float64
}

// New constructs the memory service and ensures the Qdrant collection exists,
// sized for the embedder's vector dimension (failing fast on a dimension
// mismatch with an existing collection).
func New(ctx context.Context, embedder Embedder, qdrantURL, apiKey, collection string) (*Service, error) {
	q := newQdrantClient(qdrantURL, apiKey, collection)
	if err := q.EnsureCollection(ctx, embedder.Dim()); err != nil {
		return nil, err
	}
	return &Service{embedder: embedder, qdrant: q}, nil
}

// AddSessionToMemory is a deliberate no-op. ADK defines it as ingesting an
// entire session's events, which for Quack would store unvetted drafts and tool
// chatter. The runner never calls it automatically; all writes go through
// Commit so only judge-passed findings reach the store.
func (s *Service) AddSessionToMemory(ctx context.Context, _ session.Session) error {
	return nil
}

// SearchMemory embeds the query and returns the nearest stored findings in the
// (AppName, UserID) namespace. The runner populates AppName from the agent's
// app name and UserID from the session, so an agent recalls only its own prior
// vetted findings.
//
// Recall is best-effort: a transient embedding or vector-store failure must
// never break the agent's generation (the ADK preload tool turns any error
// here into a failed LLM request). On failure we log and return no memories, so
// the agent proceeds as if memory were simply empty.
func (s *Service) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if req == nil || strings.TrimSpace(req.Query) == "" {
		return &adkmemory.SearchResponse{}, nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{req.Query})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		log.Printf("memory: recall embed failed (skipping recall): %v", err)
		return &adkmemory.SearchResponse{}, nil
	}
	hits, err := s.qdrant.Search(ctx, vecs[0], req.AppName, req.UserID, searchLimit)
	if err != nil {
		log.Printf("memory: recall search failed (skipping recall): %v", err)
		return &adkmemory.SearchResponse{}, nil
	}
	resp := &adkmemory.SearchResponse{}
	for _, h := range hits {
		finding := asString(h.Payload[fieldFinding])
		if finding == "" {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, asString(h.Payload[fieldTimestamp]))
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:             h.ID,
			Content:        &genai.Content{Role: "model", Parts: []*genai.Part{{Text: finding}}},
			Author:         asString(h.Payload[fieldAgent]),
			Timestamp:      ts,
			CustomMetadata: h.Payload,
		})
	}
	return resp, nil
}

// Commit stores one vetted finding. The point ID is derived from the namespace
// and the query, so re-committing the same agent's answer to the same question
// (judge revise loops, retries) upserts in place rather than duplicating.
func (s *Service) Commit(ctx context.Context, req CommitRequest) error {
	if strings.TrimSpace(req.Finding) == "" {
		return fmt.Errorf("memory: commit with empty finding")
	}
	vecs, err := s.embedder.Embed(ctx, []string{req.Finding})
	if err != nil {
		return err
	}
	sources := req.Sources
	if sources == nil {
		sources = []string{}
	}
	payload := map[string]any{
		fieldAppName:   req.AppName,
		fieldUserID:    req.UserID,
		fieldAgent:     req.Agent,
		fieldQuery:     req.Query,
		fieldFinding:   req.Finding,
		fieldSources:   sources,
		fieldScore:     req.Score,
		fieldTimestamp: time.Now().UTC().Format(time.RFC3339),
	}
	return s.qdrant.Upsert(ctx, qdrantPoint{
		ID:      pointID(req.AppName, req.UserID, req.Query),
		Vector:  vecs[0],
		Payload: payload,
	})
}

func asString(v any) string { s, _ := v.(string); return s }
