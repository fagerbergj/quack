package memory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/inference"
)

// OpenSQLite returns a memory Store backed by an embedded SQLite file at url (a
// path) - the no-docker path, no Qdrant container. Similarity is brute-force
// cosine in Go (no native vector extension, which would force cgo); fine for the
// hundreds–thousands of memories a single user accumulates. Multiple scopes
// (task/user) can share one file: rows are partitioned by collection + scope.
func OpenSQLite(ctx context.Context, url string, embedder inference.Embedder, consolidator model.LLM, collection, domain string, topK int, minScore float32) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(sqliteMemDSN(url)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("memory: open sqlite %q: %w", url, err)
	}
	return newStore(ctx, &sqliteIndex{db: db, coll: collection}, embedder, consolidator, collection, domain, topK, minScore)
}

// sqliteMemDSN enables WAL + a busy timeout so a memory store and any other pool
// on the same file (e.g. the session store sharing one quack.db) coordinate
// instead of failing with SQLITE_BUSY. A caller-supplied query string is honoured.
func sqliteMemDSN(url string) string {
	if strings.Contains(url, "?") {
		return url
	}
	return url + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
}

// memoryRow is one stored memory. The vector is a little-endian float32 BLOB;
// similarity is computed in Go. Rows are partitioned by (collection, scope).
type memoryRow struct {
	ID         string `gorm:"primaryKey"`
	Collection string `gorm:"index:idx_mem_scope,priority:1"`
	Scope      string `gorm:"index:idx_mem_scope,priority:2"`
	Content    string
	Author     string
	Timestamp  string
	Kind       string
	ChatID     string `gorm:"index"` // ApplyOutcome looks memories up by this
	NodeID     string
	Source     string
	MintedAt   string

	Status             string
	ValidFrom          string
	InvalidatedAt      string
	InvalidationReason string
	ReinforcementCount int

	Upvotes        int
	Downvotes      int
	VoteScore      int
	Tier           string
	LastUpvotedAt  string
	Recalls        int
	LastRecalledAt string
	HumanVote      string

	// AbsorbedIDs: comma-joined (see joinIDs/splitIDs) - epic #1255 P5.
	AbsorbedIDs string

	Vector []byte
}

func (memoryRow) TableName() string { return "memories" }

// sqliteIndex is the SQLite-backed implementation of index (brute-force cosine).
type sqliteIndex struct {
	db   *gorm.DB
	coll string
}

func (x *sqliteIndex) ensure(ctx context.Context, _ func() (int, error)) error {
	// No fixed dimension: vectors are variable-length blobs, so the embedder probe
	// is unused.
	return x.db.WithContext(ctx).AutoMigrate(&memoryRow{})
}

func (x *sqliteIndex) query(ctx context.Context, buckets []string, vec []float32, k int) ([]scored, error) {
	// Recall and the commit-path neighbour query share this: an invalidated
	// memory must never surface as a candidate to recall OR to reconcile
	// against (design doc §4(d)) - a missing/NULL status predates the
	// lifecycle fields and reads as valid.
	q := x.db.WithContext(ctx).Where("collection = ?", x.coll).
		Where("status IS NULL OR status <> ?", string(StatusInvalidated))
	if len(buckets) > 0 {
		q = q.Where("scope IN ?", buckets) // OR across the caller's buckets
	}
	var rows []memoryRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("memory: sqlite query: %w", err)
	}
	out := make([]scored, 0, len(rows))
	for _, r := range rows {
		rowVec := bytesToVec(r.Vector)
		out = append(out, scored{
			ID:                 r.ID,
			Content:            r.Content,
			Author:             r.Author,
			Timestamp:          r.Timestamp,
			Kind:               r.Kind,
			Scope:              r.Scope,
			ChatID:             r.ChatID,
			NodeID:             r.NodeID,
			Source:             r.Source,
			MintedAt:           r.MintedAt,
			Status:             r.Status,
			ValidFrom:          r.ValidFrom,
			InvalidatedAt:      r.InvalidatedAt,
			InvalidationReason: r.InvalidationReason,
			ReinforcementCount: r.ReinforcementCount,
			Upvotes:            r.Upvotes,
			Downvotes:          r.Downvotes,
			VoteScore:          r.VoteScore,
			Tier:               r.Tier,
			LastUpvotedAt:      r.LastUpvotedAt,
			Recalls:            r.Recalls,
			LastRecalledAt:     r.LastRecalledAt,
			AbsorbedIDs:        splitIDs(r.AbsorbedIDs),
			HumanVote:          r.HumanVote,
			Score:              cosine(vec, rowVec),
			Vector:             rowVec,
		})
	}
	// Highest cosine first; cap to k. ponytail: O(n) scan + sort - fine at memory
	// scale (hundreds–thousands); revisit a native index only if a corpus outgrows it.
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func (x *sqliteIndex) list(ctx context.Context, buckets []string, offset, limit int, includeInvalidated bool, tier string, sortBy ...string) ([]scored, error) {
	q := x.db.WithContext(ctx).Where("collection = ?", x.coll)
	if len(buckets) > 0 {
		q = q.Where("scope IN ?", buckets)
	}
	if !includeInvalidated {
		q = q.Where("status IS NULL OR status <> ?", string(StatusInvalidated))
	}
	q = tierWhere(q, tier)
	// id DESC breaks every ordering's ties deterministically, so paging
	// never duplicates or drops a row across offset boundaries.
	q = q.Order(sqliteOrderBy(firstSort(sortBy))).Offset(offset)
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []memoryRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("memory: sqlite list: %w", err)
	}
	out := make([]scored, len(rows))
	for i, r := range rows {
		out[i] = scored{
			ID: r.ID, Content: r.Content, Author: r.Author, Timestamp: r.Timestamp, Kind: r.Kind, Scope: r.Scope,
			ChatID: r.ChatID, NodeID: r.NodeID, Source: r.Source, MintedAt: r.MintedAt,
			Status: r.Status, ValidFrom: r.ValidFrom, InvalidatedAt: r.InvalidatedAt,
			InvalidationReason: r.InvalidationReason, ReinforcementCount: r.ReinforcementCount,
			Upvotes: r.Upvotes, Downvotes: r.Downvotes, VoteScore: r.VoteScore, Tier: r.Tier,
			LastUpvotedAt: r.LastUpvotedAt, Recalls: r.Recalls, LastRecalledAt: r.LastRecalledAt,
			AbsorbedIDs: splitIDs(r.AbsorbedIDs), HumanVote: r.HumanVote,
		}
	}
	return out, nil
}

// getByID fetches one row by id regardless of status - the caller decides
// what an invalidated point means for its purpose.
func (x *sqliteIndex) getByID(ctx context.Context, id string) (scored, bool, error) {
	var r memoryRow
	err := x.db.WithContext(ctx).Where("collection = ? AND id = ?", x.coll, id).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return scored{}, false, nil
	}
	if err != nil {
		return scored{}, false, fmt.Errorf("memory: sqlite get by id: %w", err)
	}
	return scored{
		ID: r.ID, Content: r.Content, Author: r.Author, Timestamp: r.Timestamp, Kind: r.Kind, Scope: r.Scope,
		ChatID: r.ChatID, NodeID: r.NodeID, Source: r.Source, MintedAt: r.MintedAt,
		Status: r.Status, ValidFrom: r.ValidFrom, InvalidatedAt: r.InvalidatedAt,
		InvalidationReason: r.InvalidationReason, ReinforcementCount: r.ReinforcementCount,
		Upvotes: r.Upvotes, Downvotes: r.Downvotes, VoteScore: r.VoteScore, Tier: r.Tier,
		LastUpvotedAt: r.LastUpvotedAt, Recalls: r.Recalls, LastRecalledAt: r.LastRecalledAt,
		AbsorbedIDs: splitIDs(r.AbsorbedIDs), HumanVote: r.HumanVote,
	}, true, nil
}

func (x *sqliteIndex) count(ctx context.Context, buckets []string, includeInvalidated bool, tier string) (int, error) {
	q := x.db.WithContext(ctx).Model(&memoryRow{}).Where("collection = ?", x.coll)
	if len(buckets) > 0 {
		q = q.Where("scope IN ?", buckets)
	}
	if !includeInvalidated {
		q = q.Where("status IS NULL OR status <> ?", string(StatusInvalidated))
	}
	q = tierWhere(q, tier)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, fmt.Errorf("memory: sqlite count: %w", err)
	}
	return int(n), nil
}

// sqliteOrderBy maps a ListSort constant to an ORDER BY clause, always with
// an `id DESC` tie-break (#1266) so paging never duplicates/drops a row when
// two rows share the sort column's value (e.g. two never-recalled memories
// both have last_recalled_at = ""). last_recalled sorts descending with ""
// (never recalled) last, via a CASE, not a plain string sort (empty string
// sorts before any timestamp lexically, which would put it first).
func sqliteOrderBy(sortBy string) string {
	switch sortBy {
	case SortOldest:
		return "timestamp ASC, id DESC"
	case SortScore:
		return "vote_score DESC, id DESC"
	case SortUpvotes:
		return "upvotes DESC, id DESC"
	case SortDownvotes:
		return "downvotes DESC, id DESC"
	case SortRecalls:
		return "recalls DESC, id DESC"
	case SortLastRecalled:
		return "CASE WHEN last_recalled_at IS NULL OR last_recalled_at = '' THEN 1 ELSE 0 END, last_recalled_at DESC, id DESC"
	default:
		return "timestamp DESC, id DESC"
	}
}

// tierWhere applies tier's filter to q, if any. "" means no filter.
// "unverified" also matches a row with no tier yet (empty/missing reads as
// unverified everywhere else in this package, e.g. toMemories' wire mapping).
func tierWhere(q *gorm.DB, tier string) *gorm.DB {
	switch tier {
	case "":
		return q
	case TierUnverified:
		return q.Where("tier IS NULL OR tier = '' OR tier = ?", TierUnverified)
	default:
		return q.Where("tier = ?", tier)
	}
}

func (x *sqliteIndex) upsert(ctx context.Context, pts []point) error {
	rows := make([]memoryRow, len(pts))
	for i, p := range pts {
		rows[i] = memoryRow{
			ID:                 p.ID,
			Collection:         x.coll,
			Scope:              p.Scope,
			Content:            p.Content,
			Author:             p.Author,
			Timestamp:          p.Timestamp,
			Kind:               p.Kind,
			ChatID:             p.ChatID,
			NodeID:             p.NodeID,
			Source:             p.Source,
			MintedAt:           p.MintedAt,
			Status:             p.Status,
			ValidFrom:          p.ValidFrom,
			InvalidatedAt:      p.InvalidatedAt,
			InvalidationReason: p.InvalidationReason,
			ReinforcementCount: p.ReinforcementCount,
			Upvotes:            p.Upvotes,
			Downvotes:          p.Downvotes,
			VoteScore:          p.VoteScore,
			Tier:               p.Tier,
			LastUpvotedAt:      p.LastUpvotedAt,
			Recalls:            p.Recalls,
			LastRecalledAt:     p.LastRecalledAt,
			AbsorbedIDs:        joinIDs(p.AbsorbedIDs),
			Vector:             vecToBytes(p.Vector),
		}
	}
	// Upsert on the primary key so an UPDATE op (same id) overwrites in place.
	if err := x.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("memory: sqlite upsert: %w", err)
	}
	return nil
}

func (x *sqliteIndex) remove(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := x.db.WithContext(ctx).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Delete(&memoryRow{})
	if res.Error != nil {
		return 0, fmt.Errorf("memory: sqlite delete: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// invalidateByID soft-invalidates ids in place - a plain UPDATE, never a
// DELETE (design doc §4(a): the consolidator's DELETE invalidates, it
// doesn't remove). Reports how many of ids actually existed.
func (x *sqliteIndex) invalidateByID(ctx context.Context, ids []string, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Updates(map[string]any{
			"status":              string(StatusInvalidated),
			"invalidated_at":      nowRFC3339(),
			"invalidation_reason": reason,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("memory: sqlite invalidate: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// updateStatus applies o to every id in ids that isn't already invalidated
// (sticky) and, for invalidate, isn't already tier verified (design decision
// #1255: a verified memory recalled into a closed-unmerged chat gets no
// vote). One UPDATE per row since reinforcement_count/upvotes differ per
// row. Returns the ids touched.
func (x *sqliteIndex) updateStatus(ctx context.Context, ids []string, o OutcomeSignal) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := x.db.WithContext(ctx).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Where("status IS NULL OR status <> ?", string(StatusInvalidated))
	if o.Kind == OutcomeInvalidated {
		q = q.Where("tier IS NULL OR tier <> ?", TierVerified)
	}
	var rows []memoryRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("memory: sqlite outcome query: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ts := nowRFC3339()
	touched := make([]string, len(rows))
	for i, r := range rows {
		touched[i] = r.ID
		var upd map[string]any
		switch o.Kind {
		case OutcomeReinforced:
			upd = map[string]any{
				"status": string(StatusReinforced), "reinforcement_count": r.ReinforcementCount + 1,
				"upvotes": r.Upvotes + 1, "vote_score": reinforcedVoteScore(r.Upvotes, r.Downvotes), "tier": TierVerified, "last_upvoted_at": ts,
			}
		case OutcomeInvalidated:
			upd = map[string]any{"status": string(StatusInvalidated), "invalidated_at": ts, "invalidation_reason": o.Reason}
		}
		if err := x.db.WithContext(ctx).Model(&memoryRow{}).
			Where("collection = ? AND id = ?", x.coll, r.ID).
			Updates(upd).Error; err != nil {
			return nil, fmt.Errorf("memory: sqlite outcome update: %w", err)
		}
	}
	return touched, nil
}

// applyVotes applies each vote to its memory row, skipping an
// already-invalidated one (sticky). A net score at or below
// invalidateThreshold also invalidates. Returns the ids touched.
func (x *sqliteIndex) applyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) ([]string, error) {
	if len(votes) == 0 {
		return nil, nil
	}
	ids := make([]string, len(votes))
	for i, v := range votes {
		ids[i] = v.MemoryID
	}
	var rows []memoryRow
	if err := x.db.WithContext(ctx).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Where("status IS NULL OR status <> ?", string(StatusInvalidated)).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("memory: sqlite vote query: %w", err)
	}
	byID := make(map[string]memoryRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	ts := nowRFC3339()
	var touched []string
	for _, v := range votes {
		r, ok := byID[v.MemoryID]
		if !ok {
			continue
		}
		upd := voteUpdate(r, v, ts, invalidateThreshold)
		if err := x.db.WithContext(ctx).Model(&memoryRow{}).
			Where("collection = ? AND id = ?", x.coll, r.ID).
			Updates(upd).Error; err != nil {
			return nil, fmt.Errorf("memory: sqlite vote update: %w", err)
		}
		touched = append(touched, r.ID)
	}
	return touched, nil
}

// voteUpdate builds one row's column updates from computeVoteDelta.
func voteUpdate(r memoryRow, v Vote, ts string, invalidateThreshold int) map[string]any {
	d := computeVoteDelta(r.Upvotes, r.Downvotes, r.Tier, ts, v, invalidateThreshold)
	upd := map[string]any{"upvotes": d.Upvotes, "downvotes": d.Downvotes, "vote_score": d.VoteScore, "tier": d.Tier}
	if d.LastUpvotedAt != "" {
		upd["last_upvoted_at"] = d.LastUpvotedAt
	}
	if d.Invalidate {
		upd["status"] = string(StatusInvalidated)
		upd["invalidated_at"] = ts
		upd["invalidation_reason"] = OutcomeReasonNetScore
	}
	return upd
}

// setHumanVote reads the row's current human_vote to compute the delta
// (computeHumanVoteDelta), then writes both the new vote fields and
// human_vote in one UPDATE. Returns false if id doesn't exist or is already
// invalidated (sticky, same as applyVotes).
func (x *sqliteIndex) setHumanVote(ctx context.Context, id, vote string, invalidateThreshold int) (bool, error) {
	var r memoryRow
	err := x.db.WithContext(ctx).
		Where("collection = ? AND id = ?", x.coll, id).
		Where("status IS NULL OR status <> ?", string(StatusInvalidated)).
		First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory: sqlite human-vote query: %w", err)
	}
	ts := nowRFC3339()
	d := computeHumanVoteDelta(r.Upvotes, r.Downvotes, r.Tier, r.HumanVote, vote, ts, invalidateThreshold)
	upd := map[string]any{"upvotes": d.Upvotes, "downvotes": d.Downvotes, "vote_score": d.VoteScore, "tier": d.Tier}
	if vote == HumanVoteNone {
		upd["human_vote"] = ""
	} else {
		upd["human_vote"] = vote
	}
	if d.LastUpvotedAt != "" {
		upd["last_upvoted_at"] = d.LastUpvotedAt
	}
	if d.Invalidate {
		upd["status"] = string(StatusInvalidated)
		upd["invalidated_at"] = ts
		upd["invalidation_reason"] = OutcomeReasonNetScore
	}
	if err := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id = ?", x.coll, id).
		Updates(upd).Error; err != nil {
		return false, fmt.Errorf("memory: sqlite human-vote update: %w", err)
	}
	return true, nil
}

// recordRecall bumps recalls and stamps last_recalled_at for ids in one
// batched UPDATE.
func (x *sqliteIndex) recordRecall(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	err := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Updates(map[string]any{"recalls": gorm.Expr("recalls + 1"), "last_recalled_at": nowRFC3339()}).Error
	if err != nil {
		return fmt.Errorf("memory: sqlite record recall: %w", err)
	}
	return nil
}

// backfillTiers is the one-time migration for a point with no tier yet
// (epic #1255 P1). Idempotent: only "" tier rows match, so a second boot's
// UPDATE affects zero rows. Two statements (verified/unverified) since the
// upvotes mirror value differs; both are unconditionally safe to re-run.
func (x *sqliteIndex) backfillTiers(ctx context.Context) (int, error) {
	db := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND (tier IS NULL OR tier = '')", x.coll)
	res := db.Where("reinforcement_count >= 1").
		Updates(map[string]any{"tier": TierVerified, "upvotes": gorm.Expr("reinforcement_count"), "vote_score": gorm.Expr("reinforcement_count")})
	if res.Error != nil {
		return 0, fmt.Errorf("memory: sqlite backfill verified: %w", res.Error)
	}
	touched := res.RowsAffected
	res = x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND (tier IS NULL OR tier = '') AND reinforcement_count < 1", x.coll).
		Updates(map[string]any{"tier": TierUnverified})
	if res.Error != nil {
		return 0, fmt.Errorf("memory: sqlite backfill unverified: %w", res.Error)
	}
	return int(touched + res.RowsAffected), nil
}

// updateBucket moves a row to a new bucket, unconditionally.
func (x *sqliteIndex) updateBucket(ctx context.Context, id, bucket string) error {
	if err := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id = ?", x.coll, id).
		Update("scope", bucket).Error; err != nil {
		return fmt.Errorf("memory: sqlite rescope: %w", err)
	}
	return nil
}

// absorb folds absorbedID's votes/timestamps/lineage into survivorID and
// invalidates absorbedID (epic #1255 P5). False (no-op) if either row is
// missing, or absorbedID is already invalidated (sticky).
func (x *sqliteIndex) absorb(ctx context.Context, survivorID, absorbedID, reason string) (bool, error) {
	var rows []memoryRow
	if err := x.db.WithContext(ctx).
		Where("collection = ? AND id IN ?", x.coll, []string{survivorID, absorbedID}).
		Find(&rows).Error; err != nil {
		return false, fmt.Errorf("memory: sqlite absorb query: %w", err)
	}
	var sv, ab *memoryRow
	for i := range rows {
		switch rows[i].ID {
		case survivorID:
			sv = &rows[i]
		case absorbedID:
			ab = &rows[i]
		}
	}
	if sv == nil || ab == nil || ab.Status == string(StatusInvalidated) {
		return false, nil
	}
	d := computeAbsorbDelta(
		absorbFields{Upvotes: sv.Upvotes, Downvotes: sv.Downvotes, LastUpvotedAt: sv.LastUpvotedAt, LastRecalledAt: sv.LastRecalledAt, AbsorbedIDs: splitIDs(sv.AbsorbedIDs)},
		absorbFields{Upvotes: ab.Upvotes, Downvotes: ab.Downvotes, LastUpvotedAt: ab.LastUpvotedAt, LastRecalledAt: ab.LastRecalledAt, AbsorbedIDs: splitIDs(ab.AbsorbedIDs)},
		absorbedID,
	)
	upd := map[string]any{
		"upvotes": d.Upvotes, "downvotes": d.Downvotes, "vote_score": d.VoteScore, "tier": d.Tier,
		"absorbed_ids": joinIDs(d.AbsorbedIDs),
	}
	if d.LastUpvotedAt != "" {
		upd["last_upvoted_at"] = d.LastUpvotedAt
	}
	if d.LastRecalledAt != "" {
		upd["last_recalled_at"] = d.LastRecalledAt
	}
	if err := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id = ?", x.coll, survivorID).Updates(upd).Error; err != nil {
		return false, fmt.Errorf("memory: sqlite absorb update survivor: %w", err)
	}
	ts := nowRFC3339()
	if err := x.db.WithContext(ctx).Model(&memoryRow{}).
		Where("collection = ? AND id = ?", x.coll, absorbedID).
		Updates(map[string]any{"status": string(StatusInvalidated), "invalidated_at": ts, "invalidation_reason": reason}).Error; err != nil {
		return false, fmt.Errorf("memory: sqlite absorb invalidate absorbed: %w", err)
	}
	return true, nil
}

// cosine is the cosine similarity of two equal-length vectors, in [-1, 1]; 0 for
// a length mismatch or a zero vector. Matches Qdrant's Distance_Cosine ranking.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(na))*math.Sqrt(float64(nb)))
}

func vecToBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func bytesToVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
