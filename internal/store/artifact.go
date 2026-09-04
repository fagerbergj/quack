package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
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
	TurnID string `gorm:"column:turn_id;index:idx_artifact_turn"`
	// Kind/Class/Lineage (#1090 P2): additive columns, nullable/zero-value
	// for every pre-existing row - AutoMigrate only adds columns, never
	// backfills or drops, so old revisions keep working with "" everywhere.
	// Kind = registered record kind ("code_review", "finding", ...); Class =
	// "structured" or "blob"; Lineage = JSON envelope (node_id, round,
	// parent_revision, trigger_annotation, head_sha, saved_at, author) -
	// opaque to SQL, read back only through LoadWithMeta.
	Kind      string `gorm:"column:kind"`
	Class     string `gorm:"column:class"`
	Lineage   string `gorm:"column:lineage"`
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
	dialector, err := openPostgres(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse artifact store url: %w", err)
	}
	gormCfg := &gorm.Config{Logger: slogGormLogger(), TranslateError: true}
	db, err := gorm.Open(dialector, gormCfg)
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

// artifactMetaContextKey mirrors turnIDContextKey: carries kind/class/lineage
// onto a Save through ctx, since ADK's SaveRequest has no room for them
// either (#1090 P2). Set only by SaveWithMeta.
type artifactMetaContextKey struct{}

type artifactMeta struct {
	Kind, Class string
	LineageJSON []byte
}

func withArtifactMeta(ctx context.Context, m artifactMeta) context.Context {
	return context.WithValue(ctx, artifactMetaContextKey{}, m)
}

func artifactMetaFromContext(ctx context.Context) artifactMeta {
	m, _ := ctx.Value(artifactMetaContextKey{}).(artifactMeta)
	return m
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

// SaveWithMeta is Save, stamping kind/class/lineage onto the row (#1090 P2:
// lineage lives on the row, not inside the bytes, so blob kinds carry it
// too) and turnID onto the existing turn_id column (same as SaveForTurn).
func (w *TurnAwareService) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, kind, class string, lineageJSON []byte, turnID string) (*artifact.SaveResponse, error) {
	ctx = withArtifactMeta(withTurnID(ctx, turnID), artifactMeta{Kind: kind, Class: class, LineageJSON: lineageJSON})
	return w.Service.Save(ctx, req)
}

// metaLoader is implemented only by gormArtifactService - artifact.InMemoryService()
// (used in most tests) has no row to read kind/class/lineage back from.
type metaLoader interface {
	loadMeta(ctx context.Context, req *artifact.LoadRequest) (kind, class string, lineageJSON []byte, err error)
}

// LoadWithMeta is Load, also returning kind/class/lineage when the wrapped
// service is the row-backed store; zero values otherwise (#1090 known
// ceiling: ADK's own interface carries no lineage, so a non-Postgres backend
// degrades to "no metadata" rather than erroring).
func (w *TurnAwareService) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (resp *artifact.LoadResponse, kind, class string, lineageJSON []byte, err error) {
	resp, err = w.Service.Load(ctx, req)
	if err != nil {
		return nil, "", "", nil, err
	}
	if ml, ok := w.Service.(metaLoader); ok {
		kind, class, lineageJSON, _ = ml.loadMeta(ctx, req)
	}
	return resp, kind, class, lineageJSON, nil
}

// metaUpdater is implemented only by gormArtifactService.
type metaUpdater interface {
	updateMeta(ctx context.Context, appName, userID, sessionID, name string, revision int64, kind, class string, lineageJSON []byte) error
}

// UpdateArtifactMeta overwrites one revision's kind/class/lineage - #1101's
// `quack ledger rebuild` write path, for a backend that can (the row-backed
// store); errors on artifact.InMemoryService() and similar, which have no
// row to update.
func (w *TurnAwareService) UpdateArtifactMeta(ctx context.Context, appName, userID, sessionID, name string, revision int64, kind, class string, lineageJSON []byte) error {
	mu, ok := w.Service.(metaUpdater)
	if !ok {
		return fmt.Errorf("store: artifact backend does not support metadata rebuild")
	}
	return mu.updateMeta(ctx, appName, userID, sessionID, name, revision, kind, class, lineageJSON)
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

// sessionArtifactLister is implemented by gormArtifactService; not by
// artifact.InMemoryService(), which tracks no session-wide revision history.
type sessionArtifactLister interface {
	ListForSession(ctx context.Context, appName, userID, sessionID string) ([]ArtifactSummary, error)
}

// ListForSession lists every artifact visible to a session (including
// user-scoped ones), each with its full revision history, or nil for a
// backend with no such history.
func (w *TurnAwareService) ListForSession(ctx context.Context, appName, userID, sessionID string) ([]ArtifactSummary, error) {
	l, ok := w.Service.(sessionArtifactLister)
	if !ok {
		return nil, nil
	}
	return l.ListForSession(ctx, appName, userID, sessionID)
}

var _ sessionArtifactLister = (*gormArtifactService)(nil)

// nameArtifactLister is implemented by gormArtifactService; not by
// artifact.InMemoryService(). Adversarial-review follow-up (#1094): the
// artifacts REST API's revisions endpoint used to reuse ListForSession and
// filter client-side, pulling every artifact + revision in the chat to find
// one name - this is the narrower query the same WHERE clause supports.
type nameArtifactLister interface {
	RevisionsForName(ctx context.Context, appName, userID, sessionID, name string) ([]ArtifactRevision, error)
}

// RevisionsForName lists one artifact name's revisions (ascending, like
// ListForSession's per-name slice), or nil for a backend with no such
// history. ok is false only when the backend doesn't support the query at
// all (not when the name simply has no revisions - that's an empty slice).
func (w *TurnAwareService) RevisionsForName(ctx context.Context, appName, userID, sessionID, name string) ([]ArtifactRevision, bool, error) {
	l, ok := w.Service.(nameArtifactLister)
	if !ok {
		return nil, false, nil
	}
	revs, err := l.RevisionsForName(ctx, appName, userID, sessionID, name)
	return revs, true, err
}

var _ nameArtifactLister = (*gormArtifactService)(nil)

// ArtifactSummary groups one name's revisions, oldest first - the shape the
// artifacts API lists per chat.
type ArtifactSummary struct {
	Name      string             `json:"name"`
	Revisions []ArtifactRevision `json:"revisions"`
}

// ArtifactRevision is one revision's metadata, without payload bytes - the
// shape the future artifacts API needs.
type ArtifactRevision struct {
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	TurnID    string    `json:"turn_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Kind/Class/Lineage (#1090 P2 columns): zero-value for a pre-#1090 row.
	Kind        string `json:"kind,omitempty"`
	Class       string `json:"class,omitempty"`
	LineageJSON string `json:"-"`
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
		out[i] = ArtifactRevision{Name: a.Name, Revision: a.Revision, MimeType: a.MimeType, Size: a.Size, TurnID: a.TurnID, CreatedAt: a.CreatedAt, Kind: a.Kind, Class: a.Class, LineageJSON: a.Lineage}
	}
	return out, nil
}

// Save implements [artifact.Service]. Version numbering always
// auto-increments (matches ADK's own services - both ignore an explicit
// SaveRequest.Version; see inmemory.go/gcsartifact's Save).
// artifactRevisionLocks serializes MAX(revision)+Create per (app, user,
// session, name) key within this process - two rounds of the same node, or
// two nodes, writing the same id can no longer both read the same MAX and
// have one insert silently fail the unique index (#1090 adversarial review
// finding #3). recordstore.Client's own per-(chat,id) lock (#1107) is the
// primary serializer for every write that goes through recordstore; this one
// is a defensive backstop for a caller that reaches artifact.Service
// directly, bypassing recordstore entirely (e.g. attachments, REST reads
// that Save via the raw ADK service) - kept rather than deleted because that
// path has no other lock at all. ponytail: process-local only, not a real
// distributed lock (Postgres advisory lock keyed on hashtext(...) would cover
// multiple replicas too) - the retry loop below is the cross-process safety
// net for that gap, and quack runs single-instance today.
var artifactRevisionLocks sync.Map // key -> *sync.Mutex

func revisionLockFor(appName, userID, sessionID, name string) *sync.Mutex {
	key := appName + "\x00" + userID + "\x00" + sessionID + "\x00" + name
	v, _ := artifactRevisionLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// maxRevisionInsertAttempts bounds the retry-on-unique-violation loop below -
// a losing insert (this process's mutex missed it, e.g. a second replica)
// gets a couple of fresh-MAX retries before giving up loud.
const maxRevisionInsertAttempts = 5

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

	mu := revisionLockFor(req.AppName, req.UserID, sessionID, req.FileName)
	mu.Lock()
	defer mu.Unlock()

	meta := artifactMetaFromContext(ctx)
	var rec Artifact
	for attempt := 1; ; attempt++ {
		var maxRev int64
		if err := s.db.WithContext(ctx).Model(&Artifact{}).
			Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ?", req.AppName, req.UserID, sessionID, req.FileName).
			Select("COALESCE(MAX(revision), 0)").Scan(&maxRev).Error; err != nil {
			return nil, err
		}
		rec = Artifact{
			AppName: req.AppName, UserID: req.UserID, SessionID: sessionID, Name: req.FileName,
			Revision: maxRev + 1, MimeType: mime, Size: int64(len(data)),
			LOOid: loOid, RowBlobID: rowBlobID, TurnID: turnIDFromContext(ctx),
			Kind: meta.Kind, Class: meta.Class, Lineage: string(meta.LineageJSON),
		}
		err := s.db.WithContext(ctx).Create(&rec).Error
		if err == nil {
			break
		}
		if errors.Is(err, gorm.ErrDuplicatedKey) && attempt < maxRevisionInsertAttempts {
			continue
		}
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

// loadMeta backs TurnAwareService.LoadWithMeta: same lookup as Load, minus
// the blob fetch, returning the row's kind/class/lineage instead.
func (s *gormArtifactService) loadMeta(ctx context.Context, req *artifact.LoadRequest) (kind, class string, lineageJSON []byte, err error) {
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
	if err := q.First(&a).Error; err != nil {
		return "", "", nil, err
	}
	return a.Kind, a.Class, []byte(a.Lineage), nil
}

// updateMeta overwrites one revision's kind/class/lineage in place - #1101's
// `quack ledger rebuild` write path. Bytes and revision number are never
// touched; only the metadata a fold recomputes from the WAL.
func (s *gormArtifactService) updateMeta(ctx context.Context, appName, userID, sessionID, name string, revision int64, kind, class string, lineageJSON []byte) error {
	if fileHasUserNamespace(name) {
		sessionID = userScopedArtifactKey
	}
	res := s.db.WithContext(ctx).Model(&Artifact{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND name = ? AND revision = ?", appName, userID, sessionID, name, revision).
		Updates(map[string]any{"kind": kind, "class": class, "lineage": string(lineageJSON)})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("artifact not found: %w", fs.ErrNotExist)
	}
	return nil
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

// ListForSession lists every artifact visible to the session (same
// session-or-user-scoped rule as List), each with its full revision
// history in ascending revision order - the artifacts API's list shape.
func (s *gormArtifactService) ListForSession(ctx context.Context, appName, userID, sessionID string) ([]ArtifactSummary, error) {
	var rows []Artifact
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id IN ?", appName, userID, []string{sessionID, userScopedArtifactKey}).
		Order("name ASC, revision ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	summaries := make([]ArtifactSummary, 0)
	for _, a := range rows {
		rev := ArtifactRevision{Name: a.Name, Revision: a.Revision, MimeType: a.MimeType, Size: a.Size, TurnID: a.TurnID, CreatedAt: a.CreatedAt, Kind: a.Kind, Class: a.Class, LineageJSON: a.Lineage}
		if n := len(summaries); n > 0 && summaries[n-1].Name == a.Name {
			summaries[n-1].Revisions = append(summaries[n-1].Revisions, rev)
			continue
		}
		summaries = append(summaries, ArtifactSummary{Name: a.Name, Revisions: []ArtifactRevision{rev}})
	}
	return summaries, nil
}

// RevisionsForName lists one artifact name's revisions in the session (same
// session-or-user-scoped rule as ListForSession), ascending - the WHERE name
// = ? sibling of ListForSession, for a caller that only wants one artifact's
// history instead of the whole chat's.
func (s *gormArtifactService) RevisionsForName(ctx context.Context, appName, userID, sessionID, name string) ([]ArtifactRevision, error) {
	var rows []Artifact
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id IN ? AND name = ?", appName, userID, []string{sessionID, userScopedArtifactKey}, name).
		Order("revision ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ArtifactRevision, len(rows))
	for i, a := range rows {
		out[i] = ArtifactRevision{Name: a.Name, Revision: a.Revision, MimeType: a.MimeType, Size: a.Size, TurnID: a.TurnID, CreatedAt: a.CreatedAt, Kind: a.Kind, Class: a.Class, LineageJSON: a.Lineage}
	}
	return out, nil
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
