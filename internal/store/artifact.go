package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Artifact is the durable metadata record for one artifact revision.
// Payload bytes live elsewhere: a Postgres large object (LOOid) or a
// sibling ArtifactBlob row (RowBlobID), never inline here.
type Artifact struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	AppName   string `gorm:"column:app_name;uniqueIndex:idx_artifact_rev"`
	UserID    string `gorm:"column:user_id;uniqueIndex:idx_artifact_rev"`
	SessionID string `gorm:"column:session_id;uniqueIndex:idx_artifact_rev"`
	Name      string `gorm:"column:name;uniqueIndex:idx_artifact_rev"`
	Revision  int64  `gorm:"column:revision;uniqueIndex:idx_artifact_rev"`
	MimeType  string `gorm:"column:mime_type"`
	Size      int64  `gorm:"column:size"`
	// LOOid: Postgres large-object OID (loBlobBackend), nil under the row backend.
	LOOid *uint32 `gorm:"column:lo_oid"`
	// RowBlobID: ArtifactBlob FK (rowBlobBackend), nil under the large-object backend.
	RowBlobID *uint `gorm:"column:row_blob_id"`
	// TurnID: the turn that created this revision, "" if unknown. Identity
	// is chat-level (same name, new revision across turns); this is per-revision.
	TurnID    string `gorm:"column:turn_id;index:idx_artifact_turn"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ArtifactBlob holds payload bytes for the row-backed service. One row per
// Artifact, referenced by Artifact.RowBlobID.
type ArtifactBlob struct {
	ID   uint   `gorm:"primaryKey;autoIncrement"`
	Data []byte `gorm:"column:data"`
}

// userScopedArtifactKey mirrors ADK's in-memory/gcs services: a "user:"
// filename is stored under this literal session, visible across sessions.
const userScopedArtifactKey = "user"

func fileHasUserNamespace(name string) bool { return len(name) >= 5 && name[:5] == "user:" }

// artifactBlobBackend stores/loads/deletes payload bytes for one Artifact
// row. save runs before the row exists; it returns the locator to stamp on
// the row the caller then creates.
type artifactBlobBackend interface {
	migrate(db *gorm.DB) error
	save(ctx context.Context, db *gorm.DB, data []byte) (loOid *uint32, rowBlobID *uint, err error)
	load(ctx context.Context, db *gorm.DB, a Artifact) ([]byte, error)
	// delete is a no-op (nil error) when a has no locator for this backend.
	delete(ctx context.Context, db *gorm.DB, a Artifact) error
}

// gormArtifactService is a GORM-backed adk artifact.Service. Version
// numbering, latest-by-default Load, and delete-nonexistent-not-an-error
// mirror ADK's own inmemory/gcsartifact services - load_artifacts depends on it.
type gormArtifactService struct {
	db    *gorm.DB
	blobs artifactBlobBackend
}

func newGormArtifactService(db *gorm.DB, blobs artifactBlobBackend) (artifact.Service, error) {
	if err := db.AutoMigrate(&Artifact{}); err != nil {
		return nil, fmt.Errorf("store: automigrate artifacts: %w", err)
	}
	if err := blobs.migrate(db); err != nil {
		return nil, fmt.Errorf("store: automigrate artifact blobs: %w", err)
	}
	return &gormArtifactService{db: db, blobs: blobs}, nil
}

// NewRowArtifactService returns a GORM-backed artifact.Service storing
// payload bytes in a sibling row - sqlite/tests have no large objects.
func NewRowArtifactService(db *gorm.DB) (artifact.Service, error) {
	return newGormArtifactService(db, rowBlobBackend{})
}

// NewLargeObjectArtifactService returns a GORM-backed artifact.Service
// storing payload bytes as Postgres large objects (see
// https://www.postgresql.org/docs/current/largeobjects.html). db must be postgres.
func NewLargeObjectArtifactService(db *gorm.DB) (artifact.Service, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("store: artifact service sql.DB: %w", err)
	}
	return newGormArtifactService(db, &loBlobBackend{sqlDB: sqlDB})
}

// NewArtifactService opens a dedicated Postgres connection at url and
// returns the large-object-backed artifact.Service - durable across
// restarts. url must be a postgres DSN (config.validate enforces this).
func NewArtifactService(url string) (artifact.Service, error) {
	gormCfg := &gorm.Config{Logger: slogGormLogger()}
	db, err := gorm.Open(postgres.Open(url), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("store: open artifact store: %w", err)
	}
	return NewLargeObjectArtifactService(db)
}

// RowArtifactService returns a row-backed artifact.Service sharing this
// Store's connection - test convenience; production uses NewArtifactService.
func (s *Store) RowArtifactService() (artifact.Service, error) {
	return NewRowArtifactService(s.db)
}

func partBytes(p *genai.Part) ([]byte, string) {
	if p.InlineData != nil {
		return p.InlineData.Data, p.InlineData.MIMEType
	}
	return []byte(p.Text), "text/plain"
}

// turnIDContextKey carries the creating turn onto a Save through ctx - the
// ADK artifact.Service interface has no turn parameter (not ours to fork).
type turnIDContextKey struct{}

func withTurnID(ctx context.Context, turnID string) context.Context {
	if turnID == "" {
		return ctx
	}
	return context.WithValue(ctx, turnIDContextKey{}, turnID)
}

func turnIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(turnIDContextKey{}).(string)
	return id
}

// TurnAwareService adds SaveForTurn to an artifact.Service - the entry-point
// addition. Embeds the interface, so every other method (including the
// plain turn-blind Save ADK's own runner/tools use) passes through unchanged.
type TurnAwareService struct {
	artifact.Service
}

// NewTurnAwareService wraps svc (either backend, or artifact.InMemoryService()).
func NewTurnAwareService(svc artifact.Service) *TurnAwareService {
	return &TurnAwareService{Service: svc}
}

// SaveForTurn is Save, stamping turnID onto the created revision ("" for a
// caller with no turn context).
func (w *TurnAwareService) SaveForTurn(ctx context.Context, req *artifact.SaveRequest, turnID string) (*artifact.SaveResponse, error) {
	return w.Service.Save(withTurnID(ctx, turnID), req)
}

// turnRevisionLister is implemented by gormArtifactService; not by
// artifact.InMemoryService(), which tracks no turn history.
type turnRevisionLister interface {
	RevisionsByTurn(ctx context.Context, appName, userID, sessionID, turnID string) ([]ArtifactRevision, error)
}

// RevisionsByTurn reports every artifact revision created by turnID, or nil
// for a backend with no turn history.
func (w *TurnAwareService) RevisionsByTurn(ctx context.Context, appName, userID, sessionID, turnID string) ([]ArtifactRevision, error) {
	l, ok := w.Service.(turnRevisionLister)
	if !ok {
		return nil, nil
	}
	return l.RevisionsByTurn(ctx, appName, userID, sessionID, turnID)
}

var _ turnRevisionLister = (*gormArtifactService)(nil)

// ArtifactRevision is one revision's metadata, without payload bytes - the
// shape the future artifacts API needs.
type ArtifactRevision struct {
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	TurnID    string    `json:"turn_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RevisionsByTurn returns every artifact revision created by turnID, in the
// session, ordered by name then revision.
func (s *gormArtifactService) RevisionsByTurn(ctx context.Context, appName, userID, sessionID, turnID string) ([]ArtifactRevision, error) {
	var rows []Artifact
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND turn_id = ?", appName, userID, sessionID, turnID).
		Order("name ASC, revision ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ArtifactRevision, len(rows))
	for i, a := range rows {
		out[i] = ArtifactRevision{Name: a.Name, Revision: a.Revision, MimeType: a.MimeType, Size: a.Size, TurnID: a.TurnID, CreatedAt: a.CreatedAt}
	}
	return out, nil
}

// Save implements [artifact.Service]. Version numbering always
// auto-increments (matches ADK's own services - both ignore an explicit
// SaveRequest.Version; see inmemory.go/gcsartifact's Save).
func (s *gormArtifactService) Save(ctx context.Context, req *artifact.SaveRequest) (*artifact.SaveResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}
	sessionID := req.SessionID
	if fileHasUserNamespace(req.FileName) {
		sessionID = userScopedArtifactKey
	}
	data, mime := partBytes(req.Part)

	// Blob write happens before the metadata row: a failure here orphans
	// nothing. A row-create failure after this orphans the blob - vacuumlo
	// (postgres) / a dead row (sqlite) is the accepted cost.
	loOid, rowBlobID, err := s.blobs.save(ctx, s.db, data)
	if err != nil {
		return nil, fmt.Errorf("store: save artifact blob: %w", err)
	}

	var maxRev int64
	if err := s.db.WithContext(ctx).Model(&Artifact{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?", req.AppName, req.UserID, sessionID, req.FileName).
		Select("COALESCE(MAX(revision), 0)").Scan(&maxRev).Error; err != nil {
		return nil, err
	}

	rec := Artifact{
		AppName: req.AppName, UserID: req.UserID, SessionID: sessionID, Name: req.FileName,
		Revision: maxRev + 1, MimeType: mime, Size: int64(len(data)),
		LOOid: loOid, RowBlobID: rowBlobID, TurnID: turnIDFromContext(ctx),
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return nil, err
	}
	return &artifact.SaveResponse{Version: rec.Revision}, nil
}

// Load implements [artifact.Service]. Version 0 (or unset) loads the latest revision.
func (s *gormArtifactService) Load(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}
	sessionID := req.SessionID
	if fileHasUserNamespace(req.FileName) {
		sessionID = userScopedArtifactKey
	}
	q := s.db.WithContext(ctx).Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?",
		req.AppName, req.UserID, sessionID, req.FileName)
	if req.Version > 0 {
		q = q.Where("revision = ?", req.Version)
	} else {
		q = q.Order("revision DESC")
	}
	var a Artifact
	err := q.First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("artifact not found: %w", fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	data, err := s.blobs.load(ctx, s.db, a)
	if err != nil {
		return nil, fmt.Errorf("store: load artifact blob: %w", err)
	}
	return &artifact.LoadResponse{Part: genai.NewPartFromBytes(data, a.MimeType)}, nil
}

// Delete implements [artifact.Service]. Deleting a non-existing entry is not
// an error (matches ADK's own services). Version 0 deletes every revision.
func (s *gormArtifactService) Delete(ctx context.Context, req *artifact.DeleteRequest) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("request validation failed: %w", err)
	}
	sessionID := req.SessionID
	if fileHasUserNamespace(req.FileName) {
		sessionID = userScopedArtifactKey
	}
	q := s.db.WithContext(ctx).Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?",
		req.AppName, req.UserID, sessionID, req.FileName)
	if req.Version != 0 {
		q = q.Where("revision = ?", req.Version)
	}
	var rows []Artifact
	if err := q.Find(&rows).Error; err != nil {
		return err
	}
	for _, a := range rows {
		if err := s.blobs.delete(ctx, s.db, a); err != nil {
			return fmt.Errorf("store: delete artifact blob: %w", err)
		}
		if err := s.db.WithContext(ctx).Delete(&Artifact{}, a.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// List implements [artifact.Service]: filenames visible in the session, plus
// any user-scoped ("user:"-prefixed) filenames for the same app+user.
func (s *gormArtifactService) List(ctx context.Context, req *artifact.ListRequest) (*artifact.ListResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}
	var names []string
	err := s.db.WithContext(ctx).Model(&Artifact{}).
		Where("app_name = ? AND user_id = ? AND session_id IN ?", req.AppName, req.UserID, []string{req.SessionID, userScopedArtifactKey}).
		Distinct("name").Order("name").Pluck("name", &names).Error
	if err != nil {
		return nil, err
	}
	return &artifact.ListResponse{FileNames: names}, nil
}

// Versions implements [artifact.Service] and errors if no versions are found.
func (s *gormArtifactService) Versions(ctx context.Context, req *artifact.VersionsRequest) (*artifact.VersionsResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}
	sessionID := req.SessionID
	if fileHasUserNamespace(req.FileName) {
		sessionID = userScopedArtifactKey
	}
	var revs []int64
	err := s.db.WithContext(ctx).Model(&Artifact{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?", req.AppName, req.UserID, sessionID, req.FileName).
		Order("revision ASC").Pluck("revision", &revs).Error
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, fmt.Errorf("artifact not found: %w", fs.ErrNotExist)
	}
	return &artifact.VersionsResponse{Versions: revs}, nil
}

// GetArtifactVersion implements [artifact.Service].
func (s *gormArtifactService) GetArtifactVersion(ctx context.Context, req *artifact.GetArtifactVersionRequest) (*artifact.GetArtifactVersionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("request validation failed: %w", err)
	}
	sessionID := req.SessionID
	if fileHasUserNamespace(req.FileName) {
		sessionID = userScopedArtifactKey
	}
	q := s.db.WithContext(ctx).Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?",
		req.AppName, req.UserID, sessionID, req.FileName)
	if req.Version > 0 {
		q = q.Where("revision = ?", req.Version)
	} else {
		q = q.Order("revision DESC")
	}
	var a Artifact
	err := q.First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("artifact not found: %w", fs.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return &artifact.GetArtifactVersionResponse{ArtifactVersion: &artifact.ArtifactVersion{
		Version: a.Revision, MimeType: a.MimeType, CreateTime: a.CreatedAt,
	}}, nil
}

var _ artifact.Service = (*gormArtifactService)(nil)

// --- row backend (sqlite, tests: no large objects available) ---

type rowBlobBackend struct{}

func (rowBlobBackend) migrate(db *gorm.DB) error { return db.AutoMigrate(&ArtifactBlob{}) }

func (rowBlobBackend) save(ctx context.Context, db *gorm.DB, data []byte) (*uint32, *uint, error) {
	b := ArtifactBlob{Data: data}
	if err := db.WithContext(ctx).Create(&b).Error; err != nil {
		return nil, nil, err
	}
	return nil, &b.ID, nil
}

func (rowBlobBackend) load(ctx context.Context, db *gorm.DB, a Artifact) ([]byte, error) {
	if a.RowBlobID == nil {
		return nil, fmt.Errorf("store: artifact %d has no row blob", a.ID)
	}
	var b ArtifactBlob
	if err := db.WithContext(ctx).First(&b, *a.RowBlobID).Error; err != nil {
		return nil, err
	}
	return b.Data, nil
}

func (rowBlobBackend) delete(ctx context.Context, db *gorm.DB, a Artifact) error {
	if a.RowBlobID == nil {
		return nil
	}
	return db.WithContext(ctx).Delete(&ArtifactBlob{}, *a.RowBlobID).Error
}

// --- large-object backend (postgres) ---

// loBlobBackend stores payload bytes as Postgres large objects. pgx's
// LargeObjects API needs a real pgx.Tx, so each op grabs a raw conn via
// sqlDB.Conn + (*sql.Conn).Raw - separate from any GORM tx (see Save).
type loBlobBackend struct {
	sqlDB *sql.DB
}

func (b *loBlobBackend) migrate(*gorm.DB) error { return nil } // no side table; the row IS the locator

// withTx runs fn inside a real pgx transaction on a raw connection acquired
// from the pool, committing on success and rolling back on error.
func (b *loBlobBackend) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	sc, err := b.sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire conn for large object op: %w", err)
	}
	defer sc.Close()

	var pgxConn *pgx.Conn
	if err := sc.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("store: connection is not a pgx stdlib conn (got %T)", driverConn)
		}
		pgxConn = c.Conn()
		return nil
	}); err != nil {
		return err
	}

	tx, err := pgxConn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin large object tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func (b *loBlobBackend) save(ctx context.Context, _ *gorm.DB, data []byte) (*uint32, *uint, error) {
	var oid uint32
	err := b.withTx(ctx, func(tx pgx.Tx) error {
		los := tx.LargeObjects()
		newOid, err := los.Create(ctx, 0)
		if err != nil {
			return fmt.Errorf("create large object: %w", err)
		}
		lo, err := los.Open(ctx, newOid, pgx.LargeObjectModeWrite)
		if err != nil {
			return fmt.Errorf("open large object for write: %w", err)
		}
		if _, err := lo.Write(data); err != nil {
			return fmt.Errorf("write large object: %w", err)
		}
		oid = newOid
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &oid, nil, nil
}

func (b *loBlobBackend) load(ctx context.Context, _ *gorm.DB, a Artifact) ([]byte, error) {
	if a.LOOid == nil {
		return nil, fmt.Errorf("store: artifact %d has no large object", a.ID)
	}
	var data []byte
	err := b.withTx(ctx, func(tx pgx.Tx) error {
		los := tx.LargeObjects()
		lo, err := los.Open(ctx, *a.LOOid, pgx.LargeObjectModeRead)
		if err != nil {
			return fmt.Errorf("open large object for read: %w", err)
		}
		data, err = io.ReadAll(lo)
		return err
	})
	return data, err
}

// delete unlinks the large object. Orphans from elsewhere (e.g. a crash
// between blob-write and row-create in Save) are vacuumlo's job, not this
// method's - it only covers the delete it's actually asked to do.
func (b *loBlobBackend) delete(ctx context.Context, _ *gorm.DB, a Artifact) error {
	if a.LOOid == nil {
		return nil
	}
	return b.withTx(ctx, func(tx pgx.Tx) error {
		los := tx.LargeObjects()
		return los.Unlink(ctx, *a.LOOid)
	})
}

var _ artifactBlobBackend = rowBlobBackend{}
var _ artifactBlobBackend = (*loBlobBackend)(nil)
