package memory

import (
	"context"
	"encoding/binary"
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
// path) — the no-docker path, no Qdrant container. Similarity is brute-force
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
	Vector     []byte
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

func (x *sqliteIndex) query(ctx context.Context, scope string, vec []float32, k int) ([]scored, error) {
	q := x.db.WithContext(ctx).Where("collection = ?", x.coll)
	if scope != "" {
		q = q.Where("scope = ?", scope)
	}
	var rows []memoryRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("memory: sqlite query: %w", err)
	}
	out := make([]scored, 0, len(rows))
	for _, r := range rows {
		out = append(out, scored{
			ID:        r.ID,
			Content:   r.Content,
			Author:    r.Author,
			Timestamp: r.Timestamp,
			Score:     cosine(vec, bytesToVec(r.Vector)),
		})
	}
	// Highest cosine first; cap to k. ponytail: O(n) scan + sort — fine at memory
	// scale (hundreds–thousands); revisit a native index only if a corpus outgrows it.
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func (x *sqliteIndex) upsert(ctx context.Context, pts []point) error {
	rows := make([]memoryRow, len(pts))
	for i, p := range pts {
		rows[i] = memoryRow{
			ID:         p.ID,
			Collection: x.coll,
			Scope:      p.Scope,
			Content:    p.Content,
			Author:     p.Author,
			Timestamp:  p.Timestamp,
			Kind:       p.Kind,
			Vector:     vecToBytes(p.Vector),
		}
	}
	// Upsert on the primary key so an UPDATE op (same id) overwrites in place.
	if err := x.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error; err != nil {
		return fmt.Errorf("memory: sqlite upsert: %w", err)
	}
	return nil
}

func (x *sqliteIndex) remove(ctx context.Context, ids []string) error {
	if err := x.db.WithContext(ctx).
		Where("collection = ? AND id IN ?", x.coll, ids).
		Delete(&memoryRow{}).Error; err != nil {
		return fmt.Errorf("memory: sqlite delete: %w", err)
	}
	return nil
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
