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
	payloadChatID    = "chat_id"
	payloadNodeID    = "node_id"
	payloadSource    = "source"
	payloadMintedAt  = "minted_at"

	payloadStatus             = "status"
	payloadValidFrom          = "valid_from"
	payloadInvalidatedAt      = "invalidated_at"
	payloadInvalidationReason = "invalidation_reason"
	payloadReinforcementCount = "reinforcement_count"
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
		ID:                 pointID(id),
		Content:            payloadString(payload, payloadContent),
		Author:             payloadString(payload, payloadAuthor),
		Timestamp:          payloadString(payload, payloadTimestamp),
		Kind:               payloadString(payload, payloadKind),
		Scope:              payloadString(payload, payloadScope),
		ChatID:             payloadString(payload, payloadChatID),
		NodeID:             payloadString(payload, payloadNodeID),
		Source:             payloadString(payload, payloadSource),
		MintedAt:           payloadString(payload, payloadMintedAt),
		Status:             payloadString(payload, payloadStatus),
		ValidFrom:          payloadString(payload, payloadValidFrom),
		InvalidatedAt:      payloadString(payload, payloadInvalidatedAt),
		InvalidationReason: payloadString(payload, payloadInvalidationReason),
		ReinforcementCount: payloadInt(payload, payloadReinforcementCount),
		Score:              score,
	}
}

// excludeInvalidated adds a must_not status=invalidated condition to f (or a
// fresh filter, so this composes with an empty bucket set too). A point
// minted before the lifecycle fields existed has no status key at all, which
// never matches a keyword condition - so it passes through as valid, exactly
// the "missing reads as valid" rule design doc §4(d) calls for.
func excludeInvalidated(f *qdrant.Filter) *qdrant.Filter {
	if f == nil {
		f = &qdrant.Filter{}
	}
	f.MustNot = append(f.MustNot, qdrant.NewMatch(payloadStatus, string(StatusInvalidated)))
	return f
}

func (x *qdrantIndex) query(ctx context.Context, buckets []string, vec []float32, k int) ([]scored, error) {
	limit := uint64(k)
	pts, err := x.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: x.coll,
		Query:          qdrant.NewQueryDense(vec),
		Limit:          &limit,
		// Recall and the commit-path neighbour query share this: an invalidated
		// memory must never surface as a candidate to recall OR to reconcile
		// against (design doc §4(d)) - filtered in the backend query, not a Go
		// post-filter, so it can't crowd valid points out of the top-k first.
		Filter:      excludeInvalidated(bucketFilter(buckets)),
		WithPayload: qdrant.NewWithPayload(true),
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
			payloadContent:            p.Content,
			payloadScope:              p.Scope,
			payloadAuthor:             p.Author,
			payloadTimestamp:          p.Timestamp,
			payloadChatID:             p.ChatID,
			payloadNodeID:             p.NodeID,
			payloadSource:             p.Source,
			payloadMintedAt:           p.MintedAt,
			payloadStatus:             p.Status,
			payloadValidFrom:          p.ValidFrom,
			payloadReinforcementCount: p.ReinforcementCount,
		}
		if p.Kind != "" {
			payload[payloadKind] = p.Kind
		}
		if p.InvalidatedAt != "" {
			payload[payloadInvalidatedAt] = p.InvalidatedAt
		}
		if p.InvalidationReason != "" {
			payload[payloadInvalidationReason] = p.InvalidationReason
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
	pids := idsToPointIDs(ids)
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

// idsToPointIDs converts memory ids to the qdrant client's point-id type.
func idsToPointIDs(ids []string) []*qdrant.PointId {
	pids := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pids[i] = qdrant.NewID(id)
	}
	return pids
}

// invalidateByID soft-invalidates ids in place - a payload-only SetPayload,
// never a Delete (design doc §4(a): the consolidator's DELETE invalidates,
// it doesn't remove).
func (x *qdrantIndex) invalidateByID(ctx context.Context, ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	wait := true
	_, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload: qdrant.NewValueMap(map[string]any{
			payloadStatus:             string(StatusInvalidated),
			payloadInvalidatedAt:      nowRFC3339(),
			payloadInvalidationReason: reason,
		}),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs(ids)}}},
	})
	if err != nil {
		return fmt.Errorf("memory: invalidate: %w", err)
	}
	return nil
}

// updateStatus applies o to every point whose chat_id matches and isn't
// already invalidated (sticky - see the index interface doc). Reinforcement
// count differs per point, so a bulk SetPayload can't carry it: fetch the
// candidates once, then reinforce writes one SetPayload per point while
// invalidate (a uniform payload) writes one call for all of them.
func (x *qdrantIndex) updateStatus(ctx context.Context, chatID string, o OutcomeSignal) ([]string, error) {
	filter := &qdrant.Filter{
		Must:    []*qdrant.Condition{qdrant.NewMatch(payloadChatID, chatID)},
		MustNot: []*qdrant.Condition{qdrant.NewMatch(payloadStatus, string(StatusInvalidated))},
	}
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{CollectionName: x.coll, Filter: filter, WithPayload: qdrant.NewWithPayload(true)})
	type candidate struct {
		id    string
		count int
	}
	var candidates []candidate
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("memory: scroll for outcome: %w", err)
		}
		for _, p := range pts {
			candidates = append(candidates, candidate{id: pointID(p.GetId()), count: payloadInt(p.GetPayload(), payloadReinforcementCount)})
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.id
	}

	wait := true
	switch o.Kind {
	case OutcomeInvalidated:
		if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
			CollectionName: x.coll,
			Wait:           &wait,
			Payload: qdrant.NewValueMap(map[string]any{
				payloadStatus:             string(StatusInvalidated),
				payloadInvalidatedAt:      nowRFC3339(),
				payloadInvalidationReason: o.Reason,
			}),
			PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs(ids)}}},
		}); err != nil {
			return nil, fmt.Errorf("memory: set payload invalidate: %w", err)
		}
	case OutcomeReinforced:
		for _, c := range candidates {
			if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
				CollectionName: x.coll,
				Wait:           &wait,
				Payload: qdrant.NewValueMap(map[string]any{
					payloadStatus:             string(StatusReinforced),
					payloadReinforcementCount: c.count + 1,
				}),
				PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{c.id})}}},
			}); err != nil {
				return nil, fmt.Errorf("memory: set payload reinforce: %w", err)
			}
		}
	}
	return ids, nil
}

func payloadString(payload map[string]*qdrant.Value, key string) string {
	if v, ok := payload[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

func payloadInt(payload map[string]*qdrant.Value, key string) int {
	if v, ok := payload[key]; ok {
		return int(v.GetIntegerValue())
	}
	return 0
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
