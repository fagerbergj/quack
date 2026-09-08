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

	payloadUpvotes        = "upvotes"
	payloadDownvotes      = "downvotes"
	payloadVoteScore      = "vote_score"
	payloadTier           = "tier"
	payloadLastUpvotedAt  = "last_upvoted_at"
	payloadRecalls        = "recalls"
	payloadLastRecalledAt = "last_recalled_at"
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
		Upvotes:            payloadInt(payload, payloadUpvotes),
		Downvotes:          payloadInt(payload, payloadDownvotes),
		VoteScore:          payloadInt(payload, payloadVoteScore),
		Tier:               payloadString(payload, payloadTier),
		LastUpvotedAt:      payloadString(payload, payloadLastUpvotedAt),
		Recalls:            payloadInt(payload, payloadRecalls),
		LastRecalledAt:     payloadString(payload, payloadLastRecalledAt),
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
func (x *qdrantIndex) list(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool) ([]scored, error) {
	filter := bucketFilter(buckets)
	if !includeInvalidated {
		filter = excludeInvalidated(filter)
	}
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: x.coll,
		Filter:         filter,
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

func (x *qdrantIndex) count(ctx context.Context, buckets []string, includeInvalidated bool) (int, error) {
	filter := bucketFilter(buckets)
	if !includeInvalidated {
		filter = excludeInvalidated(filter)
	}
	exact := true
	n, err := x.client.Count(ctx, &qdrant.CountPoints{CollectionName: x.coll, Filter: filter, Exact: &exact})
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
			payloadUpvotes:            p.Upvotes,
			payloadDownvotes:          p.Downvotes,
			payloadVoteScore:          p.VoteScore,
			payloadRecalls:            p.Recalls,
		}
		if p.Tier != "" {
			payload[payloadTier] = p.Tier
		}
		if p.LastUpvotedAt != "" {
			payload[payloadLastUpvotedAt] = p.LastUpvotedAt
		}
		if p.LastRecalledAt != "" {
			payload[payloadLastRecalledAt] = p.LastRecalledAt
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
// it doesn't remove). A Get precedes it, same as remove, to report how many
// of ids actually existed (SetPayload itself doesn't say).
func (x *qdrantIndex) invalidateByID(ctx context.Context, ids []string, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pids := idsToPointIDs(ids)
	existing, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: pids, WithPayload: qdrant.NewWithPayload(false)})
	if err != nil {
		return 0, fmt.Errorf("memory: get before invalidate: %w", err)
	}
	if len(existing) == 0 {
		return 0, nil
	}
	wait := true
	_, err = x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload: qdrant.NewValueMap(map[string]any{
			payloadStatus:             string(StatusInvalidated),
			payloadInvalidatedAt:      nowRFC3339(),
			payloadInvalidationReason: reason,
		}),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	})
	if err != nil {
		return 0, fmt.Errorf("memory: invalidate: %w", err)
	}
	return len(existing), nil
}

// getExisting fetches ids' current payload, skipping any that don't exist.
func (x *qdrantIndex) getExisting(ctx context.Context, ids []string) (map[string]map[string]*qdrant.Value, error) {
	pts, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: idsToPointIDs(ids), WithPayload: qdrant.NewWithPayload(true)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]*qdrant.Value, len(pts))
	for _, p := range pts {
		out[pointID(p.GetId())] = p.GetPayload()
	}
	return out, nil
}

// updateStatus applies o to every id in ids that isn't already invalidated
// (sticky - see the index interface doc), and for invalidate, isn't already
// tier verified (design decision #1255). Reinforcement count/upvotes differ
// per point, so a bulk SetPayload can't carry it: fetch the candidates once,
// then reinforce writes one SetPayload per point while invalidate (a
// uniform payload) writes one call for all of them.
func (x *qdrantIndex) updateStatus(ctx context.Context, ids []string, o OutcomeSignal) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	existing, err := x.getExisting(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for outcome: %w", err)
	}
	type candidate struct {
		id        string
		count     int
		upvotes   int
		downvotes int
	}
	var candidates []candidate
	for id, payload := range existing {
		if payloadString(payload, payloadStatus) == string(StatusInvalidated) {
			continue
		}
		if o.Kind == OutcomeInvalidated && payloadString(payload, payloadTier) == TierVerified {
			continue
		}
		candidates = append(candidates, candidate{
			id: id, count: payloadInt(payload, payloadReinforcementCount),
			upvotes: payloadInt(payload, payloadUpvotes), downvotes: payloadInt(payload, payloadDownvotes),
		})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	touched := make([]string, len(candidates))
	for i, c := range candidates {
		touched[i] = c.id
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
			PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs(touched)}}},
		}); err != nil {
			return nil, fmt.Errorf("memory: set payload invalidate: %w", err)
		}
	case OutcomeReinforced:
		ts := nowRFC3339()
		for _, c := range candidates {
			if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
				CollectionName: x.coll,
				Wait:           &wait,
				Payload: qdrant.NewValueMap(map[string]any{
					payloadStatus:             string(StatusReinforced),
					payloadReinforcementCount: c.count + 1,
					payloadUpvotes:            c.upvotes + 1,
					payloadVoteScore:          reinforcedVoteScore(c.upvotes, c.downvotes),
					payloadTier:               TierVerified,
					payloadLastUpvotedAt:      ts,
				}),
				PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{c.id})}}},
			}); err != nil {
				return nil, fmt.Errorf("memory: set payload reinforce: %w", err)
			}
		}
	}
	return touched, nil
}

// applyVotes applies each vote to its point, skipping an already-invalidated
// one (sticky). One SetPayload per point (the delta differs per point).
// ponytail: read-modify-write, not atomic - two concurrent applyVotes calls
// against the SAME memory (two rounds voting on one shared point at once)
// can both read the same upvotes/downvotes and one write clobbers the
// other, under-counting by the lost vote. This is true on BOTH backends
// here (sqlite's applyVotes is the same Find-then-Updates shape, see
// sqlite.go) - only recordRecall's counter differs cross-backend: sqlite
// increments with an atomic `recalls + 1` SQL expression, qdrant still
// reads-then-writes (no atomic increment in its payload API). Fine at
// today's call volume (one gate round at a time per memory in practice); a
// compare-and-set retry loop is the fix if concurrent votes on one memory
// ever become real.
func (x *qdrantIndex) applyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) ([]string, error) {
	if len(votes) == 0 {
		return nil, nil
	}
	ids := make([]string, len(votes))
	for i, v := range votes {
		ids[i] = v.MemoryID
	}
	existing, err := x.getExisting(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for votes: %w", err)
	}
	ts := nowRFC3339()
	wait := true
	var touched []string
	for _, v := range votes {
		payload, ok := existing[v.MemoryID]
		if !ok || payloadString(payload, payloadStatus) == string(StatusInvalidated) {
			continue
		}
		d := computeVoteDelta(payloadInt(payload, payloadUpvotes), payloadInt(payload, payloadDownvotes), payloadString(payload, payloadTier), ts, v, invalidateThreshold)
		set := map[string]any{payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadVoteScore: d.VoteScore, payloadTier: d.Tier}
		if d.LastUpvotedAt != "" {
			set[payloadLastUpvotedAt] = d.LastUpvotedAt
		}
		if d.Invalidate {
			set[payloadStatus] = string(StatusInvalidated)
			set[payloadInvalidatedAt] = ts
			set[payloadInvalidationReason] = OutcomeReasonNetScore
		}
		if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
			CollectionName: x.coll,
			Wait:           &wait,
			Payload:        qdrant.NewValueMap(set),
			PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{v.MemoryID})}}},
		}); err != nil {
			return nil, fmt.Errorf("memory: set payload vote: %w", err)
		}
		touched = append(touched, v.MemoryID)
	}
	return touched, nil
}

// recordRecall bumps recalls and stamps last_recalled_at per point. Qdrant
// has no atomic increment, so this reads current counts then writes each -
// still one Get, cheap at the handful of memories one injection delivers.
func (x *qdrantIndex) recordRecall(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	existing, err := x.getExisting(ctx, ids)
	if err != nil {
		return fmt.Errorf("memory: get for record recall: %w", err)
	}
	ts := nowRFC3339()
	wait := true
	for id, payload := range existing {
		if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
			CollectionName: x.coll,
			Wait:           &wait,
			Payload: qdrant.NewValueMap(map[string]any{
				payloadRecalls:        payloadInt(payload, payloadRecalls) + 1,
				payloadLastRecalledAt: ts,
			}),
			PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{id})}}},
		}); err != nil {
			return fmt.Errorf("memory: set payload record recall: %w", err)
		}
	}
	return nil
}

// backfillTiers is the one-time migration for a point with no tier payload
// key yet (epic #1255 P1). Idempotent: a point that already carries a tier
// is skipped in Go (Qdrant has no server-side "field absent" bulk update).
func (x *qdrantIndex) backfillTiers(ctx context.Context) (int, error) {
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{CollectionName: x.coll, WithPayload: qdrant.NewWithPayload(true)})
	wait := true
	touched := 0
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return touched, fmt.Errorf("memory: scroll for backfill: %w", err)
		}
		for _, p := range pts {
			payload := p.GetPayload()
			if _, ok := payload[payloadTier]; ok {
				continue
			}
			count := payloadInt(payload, payloadReinforcementCount)
			tier := TierUnverified
			if count >= 1 {
				tier = TierVerified
			}
			if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
				CollectionName: x.coll,
				Wait:           &wait,
				Payload:        qdrant.NewValueMap(map[string]any{payloadTier: tier, payloadUpvotes: count, payloadVoteScore: count}),
				PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: []*qdrant.PointId{p.GetId()}}}},
			}); err != nil {
				return touched, fmt.Errorf("memory: backfill set payload: %w", err)
			}
			touched++
		}
	}
	return touched, nil
}

// updateBucket moves a point to a new bucket (payload user_id), unconditionally.
func (x *qdrantIndex) updateBucket(ctx context.Context, id, bucket string) error {
	wait := true
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload:        qdrant.NewValueMap(map[string]any{payloadScope: bucket}),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{id})}}},
	}); err != nil {
		return fmt.Errorf("memory: set payload rescope: %w", err)
	}
	return nil
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
