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
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// indexBuildTimeout caps the blocking timestamp-index build at boot.
const indexBuildTimeout = 5 * time.Minute

// payloadScope's wire name stays "user_id" though it now holds a bucket key: legacy points carry
// an agent name or raw user id there, which is what lets Scope.Legacy work without a migration.
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
	payloadSupported      = "supported"
	payloadNotRelevant    = "not_relevant"
	payloadVoteScore      = "vote_score"
	payloadTier           = "tier"
	payloadLastUpvotedAt  = "last_upvoted_at"
	payloadRecalls        = "recalls"
	payloadLastRecalledAt = "last_recalled_at"

	// payloadAbsorbedIDs is comma-joined (see joinIDs/splitIDs).
	payloadAbsorbedIDs = "absorbed_ids"
	payloadHumanVote   = "human_vote"

	// payloadConsolidateFP: see scored.ConsolidateFP.
	payloadConsolidateFP = "consolidate_fp"
)

// Open connects to Qdrant at addr (host:port gRPC), creating the collection on first use with a vector
// size probed from the embedder. consolidator nil = recall-only; minScore 0 = no recall threshold.
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
		return x.ensureTimestampIndex(ctx)
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
	return x.ensureTimestampIndex(ctx)
}

// ensureTimestampIndex idempotently adds the `timestamp` payload index (also to pre-existing
// collections) that lets list() use Qdrant's native order_by for newest/oldest.
func (x *qdrantIndex) ensureTimestampIndex(ctx context.Context) error {
	info, err := x.client.GetCollectionInfo(ctx, x.coll)
	if err != nil {
		return fmt.Errorf("memory: collection info %q: %w", x.coll, err)
	}
	if _, ok := info.GetPayloadSchema()[payloadTimestamp]; ok {
		return nil
	}
	ft := qdrant.FieldType_FieldTypeDatetime
	// Wait=true: unset, the build returns once queued, not usable, and list()'s order_by
	// would race a startup that returned before the index existed.
	wait := true
	// Bounded so a huge pre-existing collection cannot hang boot indefinitely;
	// on timeout boot fails loud like every other memory startup error.
	ctx, cancel := context.WithTimeout(ctx, indexBuildTimeout)
	defer cancel()
	if _, err := x.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: x.coll,
		FieldName:      payloadTimestamp,
		FieldType:      &ft,
		Wait:           &wait,
	}); err != nil {
		return fmt.Errorf("memory: create timestamp index %q: %w", x.coll, err)
	}
	return nil
}

// bucketFilter ORs across the caller's buckets; empty buckets means no filter.
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

// pointFromPayload builds a scored point; vec is set only where the caller asked Qdrant for vectors.
func pointFromPayload(id *qdrant.PointId, payload map[string]*qdrant.Value, score float32, vec []float32) scored {
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
		Supported:          payloadInt(payload, payloadSupported),
		NotRelevant:        payloadInt(payload, payloadNotRelevant),
		VoteScore:          payloadInt(payload, payloadVoteScore),
		Tier:               payloadString(payload, payloadTier),
		LastUpvotedAt:      payloadString(payload, payloadLastUpvotedAt),
		Recalls:            payloadInt(payload, payloadRecalls),
		LastRecalledAt:     payloadString(payload, payloadLastRecalledAt),
		AbsorbedIDs:        splitIDs(payloadString(payload, payloadAbsorbedIDs)),
		HumanVote:          payloadString(payload, payloadHumanVote),
		ConsolidateFP:      payloadString(payload, payloadConsolidateFP),
		Score:              score,
		Vector:             vec,
	}
}

// excludeInvalidated adds must_not status=invalidated to f (or a fresh filter). A pre-lifecycle point
// has no status key, never matches the keyword, and so passes as valid.
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
		// Filtered in the backend, not in Go, so invalidated points can't crowd valid ones out of
		// the top-k for recall or the commit-path neighbour query.
		Filter:      excludeInvalidated(bucketFilter(buckets)),
		WithPayload: qdrant.NewWithPayload(true),
		// Recall's MMR re-rank needs each hit's own embedding for inter-hit cosine.
		WithVectors: qdrant.NewWithVectors(true),
	})
	if err != nil {
		return nil, err
	}
	out := make([]scored, 0, len(pts))
	for _, p := range pts {
		out = append(out, pointFromPayload(p.GetId(), p.GetPayload(), p.GetScore(), vectorData(p.GetVectors())))
	}
	return out, nil
}

// vectorData extracts the dense vector from a query result (nil for none or a named-vector shape).
func vectorData(v *qdrant.VectorsOutput) []float32 {
	vo := v.GetVector()
	if vo == nil {
		return nil
	}
	// Qdrant 1.19 servers populate the newer Dense oneof arm instead of the
	// deprecated top-level Data field; check both or every vector reads nil.
	if d := vo.GetData(); d != nil { //nolint:staticcheck // pre-1.19 servers populate Data; GetDense below covers 1.19+
		return d
	}
	return vo.GetDense().GetData()
}

// list serves one page. newest/oldest use listOrdered (native order_by), unless a point lacks a valid
// timestamp: order_by silently drops those, so fall back to a full scan + Go sort, as other sorts do.
func (x *qdrantIndex) list(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool, tier string, withVectors bool, sortBy ...string) ([]scored, error) {
	filter := bucketFilter(buckets)
	if !includeInvalidated {
		filter = excludeInvalidated(filter)
	}
	filter = tierFilter(filter, tier)
	sortKey := firstSort(sortBy)
	if limit > 0 && (sortKey == SortNewest || sortKey == SortOldest) {
		gap, err := x.hasMissingTimestamp(ctx, filter)
		if err != nil {
			return nil, err
		}
		if !gap {
			return x.listOrdered(ctx, filter, offset, limit, sortKey, withVectors)
		}
	}
	scroll := &qdrant.ScrollPoints{
		CollectionName: x.coll,
		Filter:         filter,
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if withVectors {
		// DedupeSweep's clustering needs each point's stored embedding, not a re-embed.
		scroll.WithVectors = qdrant.NewWithVectors(true)
	}
	it := x.client.ScrollAll(ctx, scroll)
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
			all = append(all, pointFromPayload(p.GetId(), p.GetPayload(), 0, vectorData(p.GetVectors())))
		}
	}
	sort.Slice(all, qdrantLess(all, sortKey))
	if offset >= len(all) {
		return []scored{}, nil
	}
	end := len(all)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return all[offset:end], nil
}

// datetimeRangeAll spans every date order_by can place; anything outside it (missing, "",
// unparseable) is what order_by silently drops.
var datetimeRangeAll = &qdrant.DatetimeRange{
	Gte: timestamppb.New(time.Time{}),
	Lte: timestamppb.New(time.Date(9998, 1, 1, 0, 0, 0, 0, time.UTC)),
}

// hasMissingTimestamp reports whether any point matching filter has a timestamp order_by can't place.
// It clones filter so the caller's copy, reused for the ordered scroll, isn't mutated.
func (x *qdrantIndex) hasMissingTimestamp(ctx context.Context, filter *qdrant.Filter) (bool, error) {
	gapFilter, ok := proto.Clone(filter).(*qdrant.Filter)
	if !ok || gapFilter == nil {
		gapFilter = &qdrant.Filter{}
	}
	gapFilter.MustNot = append(gapFilter.MustNot, qdrant.NewDatetimeRange(payloadTimestamp, datetimeRangeAll))
	exact := true
	n, err := x.client.Count(ctx, &qdrant.CountPoints{CollectionName: x.coll, Filter: gapFilter, Exact: &exact})
	if err != nil {
		return false, fmt.Errorf("memory: count missing timestamp: %w", err)
	}
	return n > 0, nil
}

// listOrdered scrolls offset+limit points by timestamp; resolveTieBoundary + the ID tie-break fix order_by's tie order.
// ponytail: deep offsets pull offset+limit points server-side; revisit if the UI pages past low thousands.
func (x *qdrantIndex) listOrdered(ctx context.Context, filter *qdrant.Filter, offset, limit int, sortBy string, withVectors bool) ([]scored, error) {
	dir := qdrant.Direction_Desc
	if sortBy == SortOldest {
		dir = qdrant.Direction_Asc
	}
	need := uint32(offset + limit)
	scroll := &qdrant.ScrollPoints{
		CollectionName: x.coll,
		Filter:         filter,
		WithPayload:    qdrant.NewWithPayload(true),
		Limit:          &need,
		OrderBy:        &qdrant.OrderBy{Key: payloadTimestamp, Direction: &dir},
	}
	if withVectors {
		scroll.WithVectors = qdrant.NewWithVectors(true)
	}
	pts, err := x.client.Scroll(ctx, scroll)
	if err != nil {
		return nil, fmt.Errorf("memory: scroll ordered: %w", err)
	}
	full := make([]scored, 0, len(pts))
	for _, p := range pts {
		full = append(full, pointFromPayload(p.GetId(), p.GetPayload(), 0, vectorData(p.GetVectors())))
	}
	if uint32(len(full)) == need && len(full) > 0 {
		full, err = x.resolveTieBoundary(ctx, filter, full, withVectors)
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(full, qdrantLess(full, sortBy))
	if offset >= len(full) {
		return []scored{}, nil
	}
	end := len(full)
	if offset+limit < end {
		end = offset + limit
	}
	return full[offset:end], nil
}

// resolveTieBoundary re-fetches batch's trailing equal-timestamp group in full when order_by's limit
// may have truncated it; points with a strictly better timestamp are already complete.
func (x *qdrantIndex) resolveTieBoundary(ctx context.Context, filter *qdrant.Filter, batch []scored, withVectors bool) ([]scored, error) {
	boundary := batch[len(batch)-1].Timestamp
	inBatch := 0
	for _, p := range batch {
		if p.Timestamp == boundary {
			inBatch++
		}
	}
	total, err := x.countAtTimestamp(ctx, filter, boundary)
	if err != nil {
		return nil, err
	}
	if total <= inBatch {
		return batch, nil
	}
	tied, err := x.fetchAtTimestamp(ctx, filter, boundary, withVectors)
	if err != nil {
		return nil, err
	}
	return append(batch[:len(batch)-inBatch:len(batch)-inBatch], tied...), nil
}

// countAtTimestamp counts points matching filter with `timestamp` exactly ts (keyword equality).
func (x *qdrantIndex) countAtTimestamp(ctx context.Context, filter *qdrant.Filter, ts string) (int, error) {
	f, ok := proto.Clone(filter).(*qdrant.Filter)
	if !ok || f == nil {
		f = &qdrant.Filter{}
	}
	f.Must = append(f.Must, qdrant.NewMatch(payloadTimestamp, ts))
	exact := true
	n, err := x.client.Count(ctx, &qdrant.CountPoints{CollectionName: x.coll, Filter: f, Exact: &exact})
	if err != nil {
		return 0, fmt.Errorf("memory: count at timestamp: %w", err)
	}
	return int(n), nil
}

// fetchAtTimestamp returns every point matching filter with `timestamp` exactly ts.
func (x *qdrantIndex) fetchAtTimestamp(ctx context.Context, filter *qdrant.Filter, ts string, withVectors bool) ([]scored, error) {
	f, ok := proto.Clone(filter).(*qdrant.Filter)
	if !ok || f == nil {
		f = &qdrant.Filter{}
	}
	f.Must = append(f.Must, qdrant.NewMatch(payloadTimestamp, ts))
	scroll := &qdrant.ScrollPoints{CollectionName: x.coll, Filter: f, WithPayload: qdrant.NewWithPayload(true)}
	if withVectors {
		scroll.WithVectors = qdrant.NewWithVectors(true)
	}
	it := x.client.ScrollAll(ctx, scroll)
	var out []scored
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("memory: scroll at timestamp: %w", err)
		}
		for _, p := range pts {
			out = append(out, pointFromPayload(p.GetId(), p.GetPayload(), 0, vectorData(p.GetVectors())))
		}
	}
}

// scrollAll walks the whole collection in pages of pageSize through one ScrollAll iterator,
// so the sweep is O(N) instead of re-scrolling from the start per page.
func (x *qdrantIndex) scrollAll(ctx context.Context, includeInvalidated, withVectors bool, pageSize int, fn func([]scored)) error {
	filter := bucketFilter(nil)
	if !includeInvalidated {
		filter = excludeInvalidated(filter)
	}
	limit := uint32(pageSize)
	scroll := &qdrant.ScrollPoints{
		CollectionName: x.coll,
		Filter:         filter,
		WithPayload:    qdrant.NewWithPayload(true),
		Limit:          &limit,
	}
	if withVectors {
		scroll.WithVectors = qdrant.NewWithVectors(true)
	}
	it := x.client.ScrollAll(ctx, scroll)
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("memory: scroll: %w", err)
		}
		page := make([]scored, 0, len(pts))
		for _, p := range pts {
			page = append(page, pointFromPayload(p.GetId(), p.GetPayload(), 0, vectorData(p.GetVectors())))
		}
		fn(page)
	}
}

func (x *qdrantIndex) count(ctx context.Context, buckets []string, includeInvalidated bool, tier string) (int, error) {
	filter := bucketFilter(buckets)
	if !includeInvalidated {
		filter = excludeInvalidated(filter)
	}
	filter = tierFilter(filter, tier)
	exact := true
	n, err := x.client.Count(ctx, &qdrant.CountPoints{CollectionName: x.coll, Filter: filter, Exact: &exact})
	if err != nil {
		return 0, fmt.Errorf("memory: count: %w", err)
	}
	return int(n), nil
}

func qdrantLess(all []scored, sortBy string) func(i, j int) bool {
	return func(i, j int) bool { return lessBy(sortBy, all[i].orderKey(), all[j].orderKey()) }
}

// tierFilter adds tier's condition to f; "" means no filter. "unverified" also matches a point with
// no tier key, nested as a should-OR so it composes with the caller's other conditions.
func tierFilter(f *qdrant.Filter, tier string) *qdrant.Filter {
	if tier == "" {
		return f
	}
	if f == nil {
		f = &qdrant.Filter{}
	}
	if tier == TierUnverified {
		f.Must = append(f.Must, &qdrant.Condition{
			ConditionOneOf: &qdrant.Condition_Filter{Filter: &qdrant.Filter{
				Should: []*qdrant.Condition{qdrant.NewIsEmpty(payloadTier), qdrant.NewMatchKeyword(payloadTier, TierUnverified)},
			}},
		})
		return f
	}
	f.Must = append(f.Must, qdrant.NewMatchKeyword(payloadTier, tier))
	return f
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
			payloadSupported:          p.Supported,
			payloadNotRelevant:        p.NotRelevant,
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
		if len(p.AbsorbedIDs) > 0 {
			payload[payloadAbsorbedIDs] = joinIDs(p.AbsorbedIDs)
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

// remove deletes ids and reports how many existed; Qdrant's Delete doesn't say, so a Get precedes it.
func (x *qdrantIndex) remove(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pids := idsToPointIDs(ids)
	if len(pids) == 0 { // every id was malformed - an empty Ids selector is ambiguous, not "none"
		return 0, nil
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

// idsToPointIDs drops non-UUID ids: the server rejects them with a raw parse error instead of a
// not-found, which broke the REST try-each-store fallback. Real ids are always uuid.NewString().
func idsToPointIDs(ids []string) []*qdrant.PointId {
	pids := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		pids = append(pids, qdrant.NewID(id))
	}
	return pids
}

// invalidateByID soft-invalidates ids via SetPayload, never a Delete, and reports how many existed.
func (x *qdrantIndex) invalidateByID(ctx context.Context, ids []string, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pids := idsToPointIDs(ids)
	if len(pids) == 0 { // every id was malformed - an empty Ids selector is ambiguous, not "none"
		return 0, nil
	}
	existing, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: pids, WithPayload: qdrant.NewWithPayload(false)})
	if err != nil {
		return 0, fmt.Errorf("memory: get before invalidate: %w", err)
	}
	if len(existing) == 0 {
		return 0, nil
	}
	err = x.setPayload(ctx, pids, map[string]any{
		payloadStatus:             string(StatusInvalidated),
		payloadInvalidatedAt:      nowRFC3339(),
		payloadInvalidationReason: reason,
	})
	if err != nil {
		return 0, fmt.Errorf("memory: invalidate: %w", err)
	}
	return len(existing), nil
}

// demoteTier sets tier=unverified for every id in ids currently at tier verified - a bulk
// payload-only SetPayload, same shape as invalidateByID, skipping any id already unverified.
func (x *qdrantIndex) demoteTier(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	existing, err := x.getExisting(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for demote: %w", err)
	}
	var touched []string
	for id, payload := range existing {
		if payloadString(payload, payloadTier) == TierVerified {
			touched = append(touched, id)
		}
	}
	if len(touched) == 0 {
		return nil, nil
	}
	if err := x.setPayload(ctx, idsToPointIDs(touched), map[string]any{payloadTier: TierUnverified}); err != nil {
		return nil, fmt.Errorf("memory: demote: %w", err)
	}
	return touched, nil
}

// getByID fetches one point by id regardless of status - the caller decides
// what an invalidated point means for its purpose.
func (x *qdrantIndex) getByID(ctx context.Context, id string) (scored, bool, error) {
	existing, err := x.getExisting(ctx, []string{id})
	if err != nil {
		return scored{}, false, fmt.Errorf("memory: qdrant get by id: %w", err)
	}
	payload, ok := existing[id]
	if !ok {
		return scored{}, false, nil
	}
	return pointFromPayload(qdrant.NewID(id), payload, 0, nil), true, nil
}

// getExisting fetches ids' current payload, skipping any that don't exist.
func (x *qdrantIndex) getExisting(ctx context.Context, ids []string) (map[string]map[string]*qdrant.Value, error) {
	pids := idsToPointIDs(ids)
	if len(pids) == 0 { // every id was malformed - an empty Ids selector is ambiguous, not "none"
		return map[string]map[string]*qdrant.Value{}, nil
	}
	pts, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: pids, WithPayload: qdrant.NewWithPayload(true)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]*qdrant.Value, len(pts))
	for _, p := range pts {
		out[pointID(p.GetId())] = p.GetPayload()
	}
	return out, nil
}

// updateStatus applies o to every non-invalidated id (and, for invalidate, non-verified id).
// Reinforce counts differ per point, so it writes one SetPayload each; invalidate writes one for all.
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

	switch o.Kind {
	case OutcomeInvalidated:
		if err := x.setPayload(ctx, idsToPointIDs(touched), map[string]any{
			payloadStatus:             string(StatusInvalidated),
			payloadInvalidatedAt:      nowRFC3339(),
			payloadInvalidationReason: o.Reason,
		}); err != nil {
			return nil, fmt.Errorf("memory: set payload invalidate: %w", err)
		}
	case OutcomeReinforced:
		// Never writes payloadTier: tier tracks judge/human support only.
		ts := nowRFC3339()
		for _, c := range candidates {
			if err := x.setPointPayload(ctx, c.id, map[string]any{
				payloadStatus:             string(StatusReinforced),
				payloadReinforcementCount: c.count + 1,
				payloadUpvotes:            c.upvotes + 1,
				payloadVoteScore:          reinforcedVoteScore(c.upvotes, c.downvotes),
				payloadLastUpvotedAt:      ts,
			}); err != nil {
				return nil, fmt.Errorf("memory: set payload reinforce: %w", err)
			}
		}
	}
	return touched, nil
}

// applyVotes applies each vote to its point, skipping invalidated ones. ponytail: read-modify-write,
// concurrent votes on one memory can lose one (same on sqlite); add a CAS retry loop if that gets real.
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
	var touched []string
	for _, v := range votes {
		payload, ok := existing[v.MemoryID]
		if !ok || payloadString(payload, payloadStatus) == string(StatusInvalidated) {
			continue
		}
		d := computeVoteDelta(payloadInt(payload, payloadUpvotes), payloadInt(payload, payloadDownvotes),
			payloadInt(payload, payloadSupported), payloadInt(payload, payloadNotRelevant), ts, v, invalidateThreshold)
		set := map[string]any{
			payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadSupported: d.Supported, payloadNotRelevant: d.NotRelevant,
			payloadVoteScore: d.VoteScore, payloadTier: d.Tier,
		}
		if d.LastUpvotedAt != "" {
			set[payloadLastUpvotedAt] = d.LastUpvotedAt
		}
		if d.Invalidate {
			set[payloadStatus] = string(StatusInvalidated)
			set[payloadInvalidatedAt] = ts
			set[payloadInvalidationReason] = d.InvalidateReason
		}
		if err := x.setPointPayload(ctx, v.MemoryID, set); err != nil {
			return nil, fmt.Errorf("memory: set payload vote: %w", err)
		}
		touched = append(touched, v.MemoryID)
	}
	return touched, nil
}

// setHumanVote applies the toggle-safe human vote delta in one SetPayload (same read-modify-write
// caveat as applyVotes). Reports false if id doesn't exist or is invalidated.
func (x *qdrantIndex) setHumanVote(ctx context.Context, id, vote string, invalidateThreshold int) (bool, error) {
	existing, err := x.getExisting(ctx, []string{id})
	if err != nil {
		return false, fmt.Errorf("memory: get for human vote: %w", err)
	}
	payload, ok := existing[id]
	if !ok || payloadString(payload, payloadStatus) == string(StatusInvalidated) {
		return false, nil
	}
	ts := nowRFC3339()
	d := computeHumanVoteDelta(payloadInt(payload, payloadUpvotes), payloadInt(payload, payloadDownvotes),
		payloadInt(payload, payloadSupported), payloadString(payload, payloadHumanVote), vote, ts, invalidateThreshold)
	set := map[string]any{
		payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadSupported: d.Supported,
		payloadVoteScore: d.VoteScore, payloadTier: d.Tier,
	}
	if vote == HumanVoteNone {
		set[payloadHumanVote] = ""
	} else {
		set[payloadHumanVote] = vote
	}
	if d.LastUpvotedAt != "" {
		set[payloadLastUpvotedAt] = d.LastUpvotedAt
	}
	if d.Invalidate {
		set[payloadStatus] = string(StatusInvalidated)
		set[payloadInvalidatedAt] = ts
		set[payloadInvalidationReason] = OutcomeReasonNetScore
	}
	if err := x.setPointPayload(ctx, id, set); err != nil {
		return false, fmt.Errorf("memory: set payload human vote: %w", err)
	}
	return true, nil
}

// setPointPayload writes set onto a single point with wait=true.
func (x *qdrantIndex) setPointPayload(ctx context.Context, id string, set map[string]any) error {
	return x.setPayload(ctx, idsToPointIDs([]string{id}), set)
}

// setPayload writes set onto every point in pids with wait=true.
func (x *qdrantIndex) setPayload(ctx context.Context, pids []*qdrant.PointId, set map[string]any) error {
	wait := true
	_, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload:        qdrant.NewValueMap(set),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	})
	return err
}

// recordRecall bumps recalls and stamps last_recalled_at per point (Qdrant has no atomic increment).
func (x *qdrantIndex) recordRecall(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	existing, err := x.getExisting(ctx, ids)
	if err != nil {
		return fmt.Errorf("memory: get for record recall: %w", err)
	}
	ts := nowRFC3339()
	for id, payload := range existing {
		if err := x.setPointPayload(ctx, id, map[string]any{payloadRecalls: payloadInt(payload, payloadRecalls) + 1, payloadLastRecalledAt: ts}); err != nil {
			return fmt.Errorf("memory: set payload record recall: %w", err)
		}
	}
	return nil
}

// backfillTiers sets a tier on points that have none. Idempotent; the absence check runs in Go
// because Qdrant has no server-side "field absent" bulk update.
func (x *qdrantIndex) backfillTiers(ctx context.Context) (int, error) {
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{CollectionName: x.coll, WithPayload: qdrant.NewWithPayload(true)})
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
			if err := x.setPayload(ctx, []*qdrant.PointId{p.GetId()}, map[string]any{
				payloadTier:      tier,
				payloadUpvotes:   count,
				payloadVoteScore: count,
			}); err != nil {
				return touched, fmt.Errorf("memory: backfill set payload: %w", err)
			}
			touched++
		}
	}
	return touched, nil
}

// backfillJudgeSupport sets supported = upvotes - reinforcement_count on verified points still at 0,
// demoting them to unverified when that is 0. Idempotent both ways.
func (x *qdrantIndex) backfillJudgeSupport(ctx context.Context) (int, error) {
	it := x.client.ScrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: x.coll,
		WithPayload:    qdrant.NewWithPayload(true),
		Filter:         &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword(payloadTier, TierVerified)}},
	})
	touched := 0
	for {
		pts, err := it.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return touched, fmt.Errorf("memory: scroll for judge-support backfill: %w", err)
		}
		for _, p := range pts {
			payload := p.GetPayload()
			if payloadInt(payload, payloadSupported) > 0 {
				continue
			}
			supported := payloadInt(payload, payloadUpvotes) - payloadInt(payload, payloadReinforcementCount)
			set := map[string]any{payloadSupported: 0, payloadTier: TierUnverified}
			if supported > 0 {
				set[payloadSupported], set[payloadTier] = supported, TierVerified
			}
			if err := x.setPayload(ctx, []*qdrant.PointId{p.GetId()}, set); err != nil {
				return touched, fmt.Errorf("memory: judge-support backfill set payload: %w", err)
			}
			touched++
		}
	}
	return touched, nil
}

// updateBucket moves a point to a new bucket (payload user_id), unconditionally.
func (x *qdrantIndex) updateBucket(ctx context.Context, id, bucket string) error {
	if err := x.setPointPayload(ctx, id, map[string]any{payloadScope: bucket}); err != nil {
		return fmt.Errorf("memory: set payload rescope: %w", err)
	}
	return nil
}

// stampConsolidateFP sets consolidate_fp on every id in one SetPayload call -
// a bulk payload-only mutation, no re-embed, no per-id round trip.
func (x *qdrantIndex) stampConsolidateFP(ctx context.Context, ids []string, fp string) error {
	pids := idsToPointIDs(ids)
	if len(pids) == 0 {
		return nil
	}
	if err := x.setPayload(ctx, pids, map[string]any{payloadConsolidateFP: fp}); err != nil {
		return fmt.Errorf("memory: set payload consolidate fingerprint: %w", err)
	}
	return nil
}

// absorb folds absorbedID's votes/timestamps/lineage into survivorID and invalidates absorbedID.
// False (no-op) if either point is missing or absorbedID is already invalidated.
func (x *qdrantIndex) absorb(ctx context.Context, survivorID, absorbedID, reason string) (bool, error) {
	existing, err := x.getExisting(ctx, []string{survivorID, absorbedID})
	if err != nil {
		return false, fmt.Errorf("memory: get for absorb: %w", err)
	}
	svP, ok1 := existing[survivorID]
	abP, ok2 := existing[absorbedID]
	if !ok1 || !ok2 || payloadString(abP, payloadStatus) == string(StatusInvalidated) {
		return false, nil
	}
	d := computeAbsorbDelta(
		absorbFields{
			Upvotes: payloadInt(svP, payloadUpvotes), Downvotes: payloadInt(svP, payloadDownvotes),
			Supported: payloadInt(svP, payloadSupported), NotRelevant: payloadInt(svP, payloadNotRelevant),
			LastUpvotedAt: payloadString(svP, payloadLastUpvotedAt), LastRecalledAt: payloadString(svP, payloadLastRecalledAt),
			AbsorbedIDs: splitIDs(payloadString(svP, payloadAbsorbedIDs)),
		},
		absorbFields{
			Upvotes: payloadInt(abP, payloadUpvotes), Downvotes: payloadInt(abP, payloadDownvotes),
			Supported: payloadInt(abP, payloadSupported), NotRelevant: payloadInt(abP, payloadNotRelevant),
			LastUpvotedAt: payloadString(abP, payloadLastUpvotedAt), LastRecalledAt: payloadString(abP, payloadLastRecalledAt),
			AbsorbedIDs: splitIDs(payloadString(abP, payloadAbsorbedIDs)),
		},
		absorbedID,
	)
	set := map[string]any{
		payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadSupported: d.Supported, payloadNotRelevant: d.NotRelevant,
		payloadVoteScore: d.VoteScore, payloadTier: d.Tier, payloadAbsorbedIDs: joinIDs(d.AbsorbedIDs),
	}
	if d.LastUpvotedAt != "" {
		set[payloadLastUpvotedAt] = d.LastUpvotedAt
	}
	if d.LastRecalledAt != "" {
		set[payloadLastRecalledAt] = d.LastRecalledAt
	}
	if err := x.setPointPayload(ctx, survivorID, set); err != nil {
		return false, fmt.Errorf("memory: set payload absorb survivor: %w", err)
	}
	ts := nowRFC3339()
	if err := x.setPointPayload(ctx, absorbedID, map[string]any{
		payloadStatus:             string(StatusInvalidated),
		payloadInvalidatedAt:      ts,
		payloadInvalidationReason: reason,
	}); err != nil {
		return false, fmt.Errorf("memory: set payload absorb invalidate: %w", err)
	}
	return true, nil
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
