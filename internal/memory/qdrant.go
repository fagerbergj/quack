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

// Payload keys stored on each Qdrant point. payloadScope holds the BUCKET key
// (repo:… / role:… / user:… - see scope.go). Its wire name stays "user_id": that is
// what points written before the bucket model carry (their value being an agent name
// or a raw user id), and reading them back is exactly what makes the legacy
// entitlement in Scope.Legacy work without a migration.
// indexBuildTimeout caps the blocking timestamp-index build at boot.
const indexBuildTimeout = 5 * time.Minute

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

	// payloadAbsorbedIDs: comma-joined (see joinIDs/splitIDs) - epic #1255 P5.
	payloadAbsorbedIDs = "absorbed_ids"
	payloadHumanVote   = "human_vote"

	// payloadConsolidateFP: see scored.ConsolidateFP.
	payloadConsolidateFP = "consolidate_fp"
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

// ensureTimestampIndex creates a payload index on `timestamp` if the
// collection doesn't already have one, idempotently (checked via
// GetCollectionInfo first so a pre-existing collection - created before this
// index existed - gets it too, not just brand-new ones). Item 7 of the perf
// audit: this is what lets list() use Qdrant's native order_by for
// newest/oldest instead of scrolling the whole collection into Go to sort.
func (x *qdrantIndex) ensureTimestampIndex(ctx context.Context) error {
	info, err := x.client.GetCollectionInfo(ctx, x.coll)
	if err != nil {
		return fmt.Errorf("memory: collection info %q: %w", x.coll, err)
	}
	if _, ok := info.GetPayloadSchema()[payloadTimestamp]; ok {
		return nil
	}
	ft := qdrant.FieldType_FieldTypeDatetime
	// Wait=true: CreateFieldIndexCollection's wait field defaults to false
	// unset, meaning it returns "Acknowledged" as soon as the build is
	// queued, not once it's actually usable - list()'s order_by would then
	// race a startup that returned before the index existed. Blocking here
	// keeps that race out of every caller instead of every list() call.
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

// pointFromPayload builds a scored point from its payload. vec is the point's
// own embedding, populated only where the caller asked Qdrant for it
// (query()'s MMR diversity re-rank, issue #1269) - nil elsewhere.
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
		// Recall's MMR diversity re-rank (issue #1269) needs each hit's own
		// embedding to compute inter-hit cosine - the query score alone is
		// only similarity to the QUERY vector, not to other hits.
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

// vectorData extracts the plain dense vector from a query result's
// VectorsOutput (nil if the point somehow carries none/a named-vector shape
// this collection never uses).
func vectorData(v *qdrant.VectorsOutput) []float32 {
	vo := v.GetVector()
	if vo == nil {
		return nil
	}
	// Qdrant 1.19 servers populate the newer Dense oneof arm instead of the
	// deprecated top-level Data field; check both or every vector reads nil.
	if d := vo.GetData(); d != nil {
		return d
	}
	return vo.GetDense().GetData()
}

// list serves one page. newest/oldest (the default and the only sorts the
// memory-list UI's most common paths use) go through listOrdered, which asks
// Qdrant's own `timestamp` payload index (ensureTimestampIndex) for the
// order via order_by, so only offset+limit points ever cross the wire.
// order_by silently DROPS any point missing the ordered field, unlike the Go
// sort (which puts an empty timestamp last) - real writes always stamp
// Timestamp (commit.go), but a point that somehow lacks one would vanish
// from every page, so a cheap Count guard falls back to the full scan
// whenever one exists. Qdrant has no server-side way to order by vote
// counts/recalls, so those sorts (and any offset<=0 request, where nothing
// is saved by going native) still pull every matching point and sort in Go
// - fine at memory's documented scale (hundreds-thousands).
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
		// DedupeSweep's clustering (issue #1269) needs each point's own stored
		// embedding, not a re-embed - Qdrant already has it, just ask for it.
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

// datetimeRangeAll spans every date order_by's parser can place - anything
// NOT in this range (missing, "", or not a parseable RFC3339 string, e.g. a
// test fixture's placeholder "t") is exactly what order_by silently drops.
var datetimeRangeAll = &qdrant.DatetimeRange{
	Gte: timestamppb.New(time.Time{}),
	Lte: timestamppb.New(time.Date(9998, 1, 1, 0, 0, 0, 0, time.UTC)),
}

// hasMissingTimestamp reports whether any point matching filter has a
// `timestamp` order_by can't place (missing, empty, or unparseable) -
// checked as "fails a Range covering all real dates" rather than matching
// specific bad values, so it catches every shape of bad data, not just
// empty string. Clones filter before appending so the caller's copy (about
// to be reused for the ordered scroll) isn't mutated.
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

// listOrdered fetches exactly offset+limit points (not the whole collection)
// via one Scroll call using order_by on `timestamp`, then slices off the
// last `limit`. Qdrant's OrderBy has one sort key, no secondary column, and
// its own docs say so explicitly: "When sorting is based on a non-unique
// value, it is not possible to rely on an ID offset" - order_by gives no
// guarantee about which of several equal-timestamp points lands in a
// `limit`-sized cut, or in what order, so two calls with different limits can
// disagree about a tied group straddling the cut
// (https://qdrant.tech/documentation/manage-data/points/#order-points-by-payload-key,
// go-client v1.19.0 qdrant/points.proto's OrderBy/StartFrom messages - one
// scalar `key`, no secondary field). resolveTieBoundary fixes membership at
// that cut by re-fetching the exact-match group in full when Qdrant's
// truncation might have shortchanged it; qdrantLess's existing `ID`
// tie-break (the same one the Go-sort path already uses) then makes the
// whole batch's order deterministic and reproducible across calls.
// ponytail: deep offsets still pull offset+limit points server-side (no true
// random-access skip); fine at memory's documented scale, revisit if the UI
// ever pages past low thousands.
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

// resolveTieBoundary fixes up batch's trailing group of equal-timestamp
// points (the value at batch's cut, i.e. the group order_by's `limit` may
// have truncated arbitrarily) so batch's MEMBERSHIP is correct regardless of
// which of the tied points Qdrant's Scroll happened to include. Any point in
// batch with a strictly better timestamp than the last one is guaranteed
// complete already - it beat the cutoff, so it couldn't have been excluded
// (order_by ranks by value first). Only the boundary value itself can be a
// partial slice of a larger group; if a Count for that exact value finds
// more matches than batch has, this re-fetches the whole group and splices
// it in, in place of the partial one.
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

// countAtTimestamp counts points matching filter with `timestamp` exactly
// equal to ts (an RFC3339 string already known valid, since it came off a
// successful order_by result) - an exact keyword match, not the datetime
// index used for order_by/hasMissingTimestamp, since it only needs equality.
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

// fetchAtTimestamp returns every point matching filter with `timestamp`
// exactly ts, via ScrollAll (no order_by - a plain match filter, so there's
// no per-call limit truncation to worry about here).
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

// scrollAll walks every point matching includeInvalidated across the WHOLE
// collection in pages of pageSize, calling fn once per page, using one
// native ScrollAll iterator for the entire walk. Item 7 of the perf audit:
// the sweep previously called list() per page, and list() re-ran a full
// ScrollAll from the start every time - O(N^2) in points. One iterator
// threads Qdrant's own point-ID cursor across calls instead.
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

// qdrantLess builds sort.Slice's less func for one ListSort value (#1266),
// mirroring sqliteOrderBy: an `ID` tie-break so paging is stable, and
// last_recalled treats "" (never recalled) as sorting last, not first (a
// bare string compare would put "" before any RFC3339 timestamp).
func qdrantLess(all []scored, sortBy string) func(i, j int) bool {
	switch sortBy {
	case SortOldest:
		return func(i, j int) bool {
			if all[i].Timestamp != all[j].Timestamp {
				return all[i].Timestamp < all[j].Timestamp
			}
			return all[i].ID > all[j].ID
		}
	case SortScore:
		return func(i, j int) bool {
			if all[i].VoteScore != all[j].VoteScore {
				return all[i].VoteScore > all[j].VoteScore
			}
			return all[i].ID > all[j].ID
		}
	case SortUpvotes:
		return func(i, j int) bool {
			if all[i].Upvotes != all[j].Upvotes {
				return all[i].Upvotes > all[j].Upvotes
			}
			return all[i].ID > all[j].ID
		}
	case SortDownvotes:
		return func(i, j int) bool {
			if all[i].Downvotes != all[j].Downvotes {
				return all[i].Downvotes > all[j].Downvotes
			}
			return all[i].ID > all[j].ID
		}
	case SortRecalls:
		return func(i, j int) bool {
			if all[i].Recalls != all[j].Recalls {
				return all[i].Recalls > all[j].Recalls
			}
			return all[i].ID > all[j].ID
		}
	case SortLastRecalled:
		return func(i, j int) bool {
			iEmpty, jEmpty := all[i].LastRecalledAt == "", all[j].LastRecalledAt == ""
			if iEmpty != jEmpty {
				return jEmpty // non-empty sorts before empty
			}
			if all[i].LastRecalledAt != all[j].LastRecalledAt {
				return all[i].LastRecalledAt > all[j].LastRecalledAt
			}
			return all[i].ID > all[j].ID
		}
	default: // SortNewest
		return func(i, j int) bool {
			if all[i].Timestamp != all[j].Timestamp {
				return all[i].Timestamp > all[j].Timestamp
			}
			return all[i].ID > all[j].ID
		}
	}
}

// tierFilter adds tier's condition to f (or a fresh filter), if any. "" means
// no filter. "unverified" also matches a point with no tier payload key yet
// (empty/missing reads as unverified everywhere else in this package,
// #1265 review finding 10) - a should-match OR between "no tier key" and
// "tier == unverified", nested as a sub-filter so it composes with the
// caller's other must/must-not conditions.
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

// remove deletes ids and reports how many actually existed. Qdrant's Delete
// itself doesn't say - it just acknowledges the operation - so a Get precedes
// it to make the count (and Forget's 404-on-unknown-id) truthful.
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

// idsToPointIDs converts memory ids to the qdrant client's point-id type,
// dropping any id that isn't a UUID: qdrant.NewID does no client-side
// validation, and a malformed id (a stale/typo'd REST path segment, or a
// hallucinated id in a judge's vote) otherwise reaches the server and comes
// back as a raw "Unable to parse UUID" gRPC error - every real memory id is
// minted via uuid.NewString() (commit.go), so this can never drop a genuine
// one. Bug found via #1268's live harness: that raw error broke
// findMemoryByID/invalidateMemory's try-each-store fallback in
// internal/server/rest/memory.go, which only treats ErrMemoryNotFound as
// "try the next store" - on qdrant it aborted with a 500 instead of the
// clean 404 sqlite's WHERE-clause miss already gave for free.
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

// invalidateByID soft-invalidates ids in place - a payload-only SetPayload,
// never a Delete (design doc §4(a): the consolidator's DELETE invalidates,
// it doesn't remove). A Get precedes it, same as remove, to report how many
// of ids actually existed (SetPayload itself doesn't say).
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

// setHumanVote reads id's current payload (for its prior human_vote and vote
// counts), computes the toggle-safe delta, and writes both the vote fields
// and human_vote in one SetPayload. Same read-modify-write caveat as
// applyVotes above. Reports false if id doesn't exist or is invalidated.
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
		payloadString(payload, payloadTier), payloadString(payload, payloadHumanVote), vote, ts, invalidateThreshold)
	set := map[string]any{payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadVoteScore: d.VoteScore, payloadTier: d.Tier}
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
	wait := true
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload:        qdrant.NewValueMap(set),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{id})}}},
	}); err != nil {
		return false, fmt.Errorf("memory: set payload human vote: %w", err)
	}
	return true, nil
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

// stampConsolidateFP sets consolidate_fp on every id in one SetPayload call -
// a bulk payload-only mutation, no re-embed, no per-id round trip.
func (x *qdrantIndex) stampConsolidateFP(ctx context.Context, ids []string, fp string) error {
	pids := idsToPointIDs(ids)
	if len(pids) == 0 {
		return nil
	}
	wait := true
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll,
		Wait:           &wait,
		Payload:        qdrant.NewValueMap(map[string]any{payloadConsolidateFP: fp}),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: pids}}},
	}); err != nil {
		return fmt.Errorf("memory: set payload consolidate fingerprint: %w", err)
	}
	return nil
}

// absorb folds absorbedID's votes/timestamps/lineage into survivorID and
// invalidates absorbedID (epic #1255 P5). False (no-op) if either point is
// missing, or absorbedID is already invalidated (sticky).
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
			LastUpvotedAt: payloadString(svP, payloadLastUpvotedAt), LastRecalledAt: payloadString(svP, payloadLastRecalledAt),
			AbsorbedIDs: splitIDs(payloadString(svP, payloadAbsorbedIDs)),
		},
		absorbFields{
			Upvotes: payloadInt(abP, payloadUpvotes), Downvotes: payloadInt(abP, payloadDownvotes),
			LastUpvotedAt: payloadString(abP, payloadLastUpvotedAt), LastRecalledAt: payloadString(abP, payloadLastRecalledAt),
			AbsorbedIDs: splitIDs(payloadString(abP, payloadAbsorbedIDs)),
		},
		absorbedID,
	)
	wait := true
	set := map[string]any{
		payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadVoteScore: d.VoteScore, payloadTier: d.Tier,
		payloadAbsorbedIDs: joinIDs(d.AbsorbedIDs),
	}
	if d.LastUpvotedAt != "" {
		set[payloadLastUpvotedAt] = d.LastUpvotedAt
	}
	if d.LastRecalledAt != "" {
		set[payloadLastRecalledAt] = d.LastRecalledAt
	}
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll, Wait: &wait, Payload: qdrant.NewValueMap(set),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{survivorID})}}},
	}); err != nil {
		return false, fmt.Errorf("memory: set payload absorb survivor: %w", err)
	}
	ts := nowRFC3339()
	if _, err := x.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: x.coll, Wait: &wait,
		Payload:        qdrant.NewValueMap(map[string]any{payloadStatus: string(StatusInvalidated), payloadInvalidatedAt: ts, payloadInvalidationReason: reason}),
		PointsSelector: &qdrant.PointsSelector{PointsSelectorOneOf: &qdrant.PointsSelector_Points{Points: &qdrant.PointsIdsList{Ids: idsToPointIDs([]string{absorbedID})}}},
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
