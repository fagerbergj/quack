package docstore

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"github.com/fagerbergj/quack/internal/inference"
)

// VectorIndex is the semantic (dense-vector) index over document chunks. The
// reference adapter is Qdrant. Index chunks + embeds + upserts a document's
// content; Search embeds the query and returns the nearest chunks. The embedder
// is held by the adapter so callers pass plain text. Dense-only for now; hybrid
// (dense + sparse/BM25) is a future refinement — keyword search is already
// covered by the FTS index.
type VectorIndex interface {
	Index(ctx context.Context, docID, content string) error
	Search(ctx context.Context, query string, topK int) ([]VectorHit, error)
}

// VectorHit is one chunk match: which document, the chunk text, and its score.
type VectorHit struct {
	DocID string  `json:"doc_id"`
	Chunk string  `json:"chunk"`
	Score float32 `json:"score"`
}

const (
	vecPayloadDocID = "doc_id"
	vecPayloadChunk = "chunk"
)

// NewVector selects the vector-index adapter for kind (default: qdrant) and opens
// it. addr is the store URL (Qdrant gRPC host:port), collection the namespace
// (default "documents"), embedder the model used to vectorize chunks + queries.
func NewVector(kind, addr, collection string, embedder inference.Embedder) (VectorIndex, error) {
	if kind == "" {
		kind = "qdrant"
	}
	switch kind {
	case "qdrant":
		if addr == "" {
			return nil, fmt.Errorf("docstore: qdrant url is empty")
		}
		if embedder == nil {
			return nil, fmt.Errorf("docstore: vector index requires an embedder")
		}
		if collection == "" {
			collection = "documents"
		}
		return newQdrantVector(addr, collection, embedder)
	default:
		return nil, fmt.Errorf("docstore: unsupported vector kind %q", kind)
	}
}

type qdrantVector struct {
	client   *qdrant.Client
	coll     string
	embedder inference.Embedder
}

func newQdrantVector(addr, collection string, embedder inference.Embedder) (*qdrantVector, error) {
	host, port, err := parseQdrantAddr(addr)
	if err != nil {
		return nil, err
	}
	client, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		return nil, fmt.Errorf("docstore: qdrant client: %w", err)
	}
	q := &qdrantVector{client: client, coll: collection, embedder: embedder}
	if err := q.ensureCollection(context.Background()); err != nil {
		return nil, err
	}
	return q, nil
}

// ensureCollection creates the collection on first use, probing the embedder for
// the vector dimension (no hardcoded model dimension).
func (q *qdrantVector) ensureCollection(ctx context.Context) error {
	exists, err := q.client.CollectionExists(ctx, q.coll)
	if err != nil {
		return fmt.Errorf("docstore: collection exists %q: %w", q.coll, err)
	}
	if exists {
		return nil
	}
	vecs, err := q.embedder.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return fmt.Errorf("docstore: embed probe: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return fmt.Errorf("docstore: embed probe returned no vector")
	}
	if err := q.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: q.coll,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: uint64(len(vecs[0])), Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		return fmt.Errorf("docstore: create collection %q: %w", q.coll, err)
	}
	return nil
}

// Index replaces a document's chunks: it deletes any existing points for docID,
// then chunks + embeds + upserts the new content (so re-indexing on update is
// safe). A document that chunks to nothing leaves the index empty for that id.
func (q *qdrantVector) Index(ctx context.Context, docID, content string) error {
	if err := q.deleteDoc(ctx, docID); err != nil {
		return err
	}
	chunks := chunkMarkdown(content, defaultChunkSize, defaultChunkOverlap)
	if len(chunks) == 0 {
		return nil
	}
	vecs, err := q.embedder.Embed(ctx, chunks)
	if err != nil {
		return fmt.Errorf("docstore: embed chunks: %w", err)
	}
	if len(vecs) != len(chunks) {
		return fmt.Errorf("docstore: embedder returned %d vectors for %d chunks", len(vecs), len(chunks))
	}
	points := make([]*qdrant.PointStruct, len(chunks))
	for i, c := range chunks {
		points[i] = &qdrant.PointStruct{
			Id:      qdrant.NewID(uuid.NewString()),
			Vectors: qdrant.NewVectorsDense(vecs[i]),
			Payload: qdrant.NewValueMap(map[string]any{vecPayloadDocID: docID, vecPayloadChunk: c}),
		}
	}
	wait := true
	if _, err := q.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: q.coll, Wait: &wait, Points: points}); err != nil {
		return fmt.Errorf("docstore: upsert chunks: %w", err)
	}
	return nil
}

// deleteDoc removes all chunks for a document id (a no-op when none exist).
func (q *qdrantVector) deleteDoc(ctx context.Context, docID string) error {
	wait := true
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.coll,
		Wait:           &wait,
		Points: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Filter{
			Filter: &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword(vecPayloadDocID, docID)}},
		}},
	})
	if err != nil {
		return fmt.Errorf("docstore: delete chunks for %q: %w", docID, err)
	}
	return nil
}

func (q *qdrantVector) Search(ctx context.Context, query string, topK int) ([]VectorHit, error) {
	if topK <= 0 {
		topK = 5
	}
	vecs, err := q.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("docstore: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	limit := uint64(topK)
	pts, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.coll,
		Query:          qdrant.NewQueryDense(vecs[0]),
		Limit:          &limit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("docstore: query %q: %w", q.coll, err)
	}
	hits := make([]VectorHit, 0, len(pts))
	for _, p := range pts {
		pl := p.GetPayload()
		hits = append(hits, VectorHit{
			DocID: pl[vecPayloadDocID].GetStringValue(),
			Chunk: pl[vecPayloadChunk].GetStringValue(),
			Score: p.GetScore(),
		})
	}
	return hits, nil
}

func parseQdrantAddr(raw string) (string, int, error) {
	host, p, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("docstore: qdrant url must be host:port: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("docstore: bad qdrant port %q: %w", p, err)
	}
	return host, port, nil
}
