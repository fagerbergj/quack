package memory

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// Payload keys stored on each Qdrant point. payloadScope holds the BUCKET key
// (repo:… / role:… / user:… — see scope.go). Its wire name stays "user_id": that is
// what points written before the bucket model carry (their value being an agent name
// or a raw user id), and reading them back is exactly what makes the legacy
// entitlement in Scope.Legacy work without a migration.
const (
	payloadContent   = "content"
	payloadScope     = "user_id"
	payloadAuthor    = "author"
	payloadTimestamp = "timestamp"
)

// Open connects to Qdrant at addr (host:port gRPC) and returns a memory Store
// backed by it, ensuring the scope's collection exists (created on first use with
// a vector size probed from the embedder, so the model's dimension need not be
// configured). The consolidator LLM drives Commit; pass nil for a recall-only
// store. domain ("task" | "user") selects the consolidation prompt. minScore drops
// recall hits below that cosine similarity (0 = no threshold).
func Open(ctx context.Context, addr string, embedder inference.Embedder, consolidator model.LLM, collection, domain string, topK int, minScore float32) (*Store, error) {
	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	client, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		return nil, fmt.Errorf("memory: qdrant client: %w", err)
	}
	return newStore(ctx, &qdrantIndex{client: client, coll: collection}, embedder, consolidator, collection, domain, topK, minScore)
}

// qdrantIndex is the Qdrant-backed implementation of index.
type qdrantIndex struct {
	client *qdrant.Client
	coll   string
}

func (x *qdrantIndex) ensure(ctx context.Context, probeDim func() (int, error)) error {
	exists, err := x.client.CollectionExists(ctx, x.coll)
	if err != nil {
		return fmt.Errorf("memory: collection exists %q: %w", x.coll, err)
	}
	if exists {
		return nil
	}
	dim, err := probeDim()
	if err != nil {
		return err
	}
	if err := x.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: x.coll,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: uint64(dim), Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		return fmt.Errorf("memory: create collection %q: %w", x.coll, err)
	}
	return nil
}

func (x *qdrantIndex) query(ctx context.Context, buckets []string, vec []float32, k int) ([]scored, error) {
	var flt *qdrant.Filter
	if len(buckets) > 0 {
		// OR across the caller's buckets (`should` = at least one must match): a
		// coding agent reads repo:<repo> ∪ role:coding ∪ user:<id> ∪ its legacy key.
		should := make([]*qdrant.Condition, len(buckets))
		for i, b := range buckets {
			should[i] = qdrant.NewMatchKeyword(payloadScope, b)
		}
		flt = &qdrant.Filter{Should: should}
	}
	limit := uint64(k)
	pts, err := x.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: x.coll,
		Query:          qdrant.NewQueryDense(vec),
		Limit:          &limit,
		Filter:         flt,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, err
	}
	out := make([]scored, 0, len(pts))
	for _, p := range pts {
		out = append(out, scored{
			ID:        pointID(p.GetId()),
			Content:   payloadString(p.GetPayload(), payloadContent),
			Author:    payloadString(p.GetPayload(), payloadAuthor),
			Timestamp: payloadString(p.GetPayload(), payloadTimestamp),
			Score:     p.GetScore(),
		})
	}
	return out, nil
}

func (x *qdrantIndex) upsert(ctx context.Context, pts []point) error {
	points := make([]*qdrant.PointStruct, 0, len(pts))
	for _, p := range pts {
		payload := map[string]any{
			payloadContent:   p.Content,
			payloadScope:     p.Scope,
			payloadAuthor:    p.Author,
			payloadTimestamp: p.Timestamp,
		}
		if p.Kind != "" {
			payload["kind"] = p.Kind
		}
		points = append(points, &qdrant.PointStruct{
			Id:      qdrant.NewID(p.ID),
			Vectors: qdrant.NewVectorsDense(p.Vector),
			Payload: qdrant.NewValueMap(payload),
		})
	}
	wait := true
	if _, err := x.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: x.coll, Wait: &wait, Points: points}); err != nil {
		return fmt.Errorf("memory: upsert: %w", err)
	}
	return nil
}

func (x *qdrantIndex) remove(ctx context.Context, ids []string) error {
	pids := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pids[i] = qdrant.NewID(id)
	}
	wait := true
	if _, err := x.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Points:         &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	}); err != nil {
		return fmt.Errorf("memory: delete: %w", err)
	}
	return nil
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
		return "", 0, fmt.Errorf("memory: QUACK_QDRANT_URL must be host:port: %w", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("memory: bad port %q: %w", p, err)
	}
	return host, port, nil
}
