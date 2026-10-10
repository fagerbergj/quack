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

func (x *qdrantIndex) getMany(ctx context.Context, ids []string) (map[string]scored, error) {
	out := map[string]scored{}
	pids := idsToPointIDs(ids)
	if len(pids) == 0 { // every id was malformed - an empty Ids selector is ambiguous, not "none"
		return out, nil
	}
	pts, err := x.client.Get(ctx, &qdrant.GetPoints{CollectionName: x.coll, Ids: pids, WithPayload: qdrant.NewWithPayload(true)})
	if err != nil {
		return nil, fmt.Errorf("memory: get: %w", err)
	}
	for _, p := range pts {
		out[pointID(p.GetId())] = pointFromPayload(p.GetId(), p.GetPayload(), 0, nil)
	}
	return out, nil
}

// patch is one SetPayload with wait=true, so the write is visible before it returns.
func (x *qdrantIndex) patch(ctx context.Context, ids []string, set map[string]any) error {
	pids := idsToPointIDs(ids)
	if len(pids) == 0 {
		return nil
	}
	wait := true
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload:        qdrant.NewValueMap(set),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	}); err != nil {
		return fmt.Errorf("memory: set payload: %w", err)
	}
	return nil
}

// recordRecall bumps recalls and stamps last_recalled_at per point (Qdrant has no atomic increment).
func (x *qdrantIndex) recordRecall(ctx context.Context, ids []string) error {
	existing, err := x.getMany(ctx, ids)
	if err != nil {
		return fmt.Errorf("memory: get for record recall: %w", err)
	}
	ts := nowRFC3339()
	for id, p := range existing {
		if err := x.patch(ctx, []string{id}, map[string]any{payloadRecalls: p.Recalls + 1, payloadLastRecalledAt: ts}); err != nil {
			return fmt.Errorf("memory: record recall: %w", err)
		}
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
