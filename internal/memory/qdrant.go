package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// Payload keys stored on each Qdrant point. payloadScope holds the BUCKET key
// (repo:… / role:… / user:… - see scope.go). Its wire name stays "user_id": that is
// what points written before the bucket model carry (their value being an agent name
// or a raw user id), and reading them back is exactly what makes the legacy
// entitlement in Scope.Legacy work without a migration.
const (
	payloadContent   = "content"
	payloadScope     = "user_id"
	payloadAuthor    = "author"
	payloadTimestamp = "timestamp"
	payloadKind      = "kind"
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

// bucketFilter ORs across the caller's buckets (`should` = at least one must
// match): a coding agent reads repo:<repo> ∪ role:coding ∪ user:<id> ∪ its
// legacy key. Empty buckets means no filter (every point in the collection).
func bucketFilter(buckets []string) *qdrant.Filter {
	if len(buckets) == 0 {
		return nil
	}
	should := make([]*qdrant.Condition, len(buckets))
	for i, b := range buckets {
		should[i] = qdrant.NewMatchKeyword(payloadScope, b)
	}
	return &qdrant.Filter{Should: should}
}

func pointFromPayload(id *qdrant.PointId, payload map[string]*qdrant.Value, score float32) scored {
	return scored{
		ID:        pointID(id),
		Content:   payloadString(payload, payloadContent),
		Author:    payloadString(payload, payloadAuthor),
		Timestamp: payloadString(payload, payloadTimestamp),
		Kind:      payloadString(payload, payloadKind),
		Scope:     payloadString(payload, payloadScope),
		Score:     score,
	}
}

func (x *qdrantIndex) query(ctx context.Context, buckets []string, vec []float32, k int) ([]scored, error) {
	limit := uint64(k)
	pts, err := x.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: x.coll,
		Query:          qdrant.NewQueryDense(vec),
		Limit:          &limit,
		Filter:         bucketFilter(buckets),
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, err
	}
	out := make([]scored, 0, len(pts))
	for _, p := range pts {
		out = append(out, pointFromPayload(p.GetId(), p.GetPayload(), p.GetScore()))
	}
	return out, nil
}

// list fetches every point matching buckets via Scroll (paginating internally
// through ScrollAll - Qdrant's cursor has no integer offset), sorts newest
// first in Go, then slices out the requested page. Fine at memory's documented
// scale (hundreds-thousands); avoids requiring a payload index on `timestamp`
// for Qdrant's order_by, which a fresh collection won't have.
func (x *qdrantIndex) list(ctx context.Context, buckets []string, offset, limit int) ([]scored, error) {
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: x.coll,
		Filter:         bucketFilter(buckets),
		WithPayload:    qdrant.NewWithPayload(true),
	})
	var all []scored
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("memory: scroll: %w", err)
		}
		for _, p := range pts {
			all = append(all, pointFromPayload(p.GetId(), p.GetPayload(), 0))
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Timestamp != all[j].Timestamp {
			return all[i].Timestamp > all[j].Timestamp
		}
		return all[i].ID > all[j].ID
	})
	if offset >= len(all) {
		return []scored{}, nil
	}
	end := len(all)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return all[offset:end], nil
}

func (x *qdrantIndex) count(ctx context.Context, buckets []string) (int, error) {
	exact := true
	n, err := x.client.Count(ctx, &qdrant.CountPoints{CollectionName: x.coll, Filter: bucketFilter(buckets), Exact: &exact})
	if err != nil {
		return 0, fmt.Errorf("memory: count: %w", err)
	}
	return int(n), nil
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
			payload[payloadKind] = p.Kind
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

// remove deletes ids and reports how many actually existed. Qdrant's Delete
// itself doesn't say - it just acknowledges the operation - so a Get precedes
// it to make the count (and Forget's 404-on-unknown-id) truthful.
func (x *qdrantIndex) remove(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pids := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pids[i] = qdrant.NewID(id)
	}
	existing, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: pids, WithPayload: qdrant.NewWithPayload(false)})
	if err != nil {
		return 0, fmt.Errorf("memory: get before delete: %w", err)
	}
	if len(existing) == 0 {
		return 0, nil
	}
	wait := true
	if _, err := x.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Points:         &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	}); err != nil {
		return 0, fmt.Errorf("memory: delete: %w", err)
	}
	return len(existing), nil
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
