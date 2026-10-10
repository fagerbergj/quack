package pluginreg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/fagerbergj/quack/internal/pgdial"
	"github.com/fagerbergj/quack/internal/sqlitedsn"
)

// newGormCfg builds a fresh *gorm.Config per call: gorm.DB embeds *Config, so a shared one repoints every
// earlier *gorm.DB at the latest backend opened.
func newGormCfg() *gorm.Config {
	return &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
}

// OpenDB opens a *gorm.DB for kind+url, mirroring store's dialectors: store depends on this package via
// config, so importing it would cycle.
func OpenDB(kind, url string) (*gorm.DB, error) {
	switch kind {
	case "postgres":
		dialector, err := pgdial.Open(url)
		if err != nil {
			return nil, fmt.Errorf("pluginreg: parse postgres url: %w", err)
		}
		return gorm.Open(dialector, newGormCfg())
	case "sqlite":
		sqlDB, err := sql.Open(sqlite.DriverName, sqlitedsn.Build(url))
		if err != nil {
			return nil, fmt.Errorf("pluginreg: open sqlite: %w", err)
		}
		sqlDB.SetMaxOpenConns(1) // one writer; avoid SQLITE_BUSY across the pool
		return gorm.Open(&sqlite.Dialector{Conn: sqlDB}, newGormCfg())
	default:
		return nil, fmt.Errorf("pluginreg: unsupported store kind %q (postgres or sqlite)", kind)
	}
}

// PluginRow is a registry row's DB shape, the same fields as FSRegistry's entry.json; clones stay on disk.
type PluginRow struct {
	Name         string `gorm:"primaryKey"`
	Entry        string
	Source       string
	Owner        string
	Repo         string
	Ref          string
	Path         string
	InstalledSHA string
	FetchedAt    *time.Time
	Error        string
	Seeded       bool `gorm:"not null;default:false"`
	UpdatedAt    time.Time
}

func rowFromPlugin(p Plugin) PluginRow {
	return PluginRow{
		// UpdatedAt is left zero - GORM's naming convention auto-populates
		// it on Create/Update, same as every other model in internal/store.
		Name: p.Name, Entry: p.Entry, Source: p.Source, Owner: p.Owner, Repo: p.Repo,
		Ref: p.Ref, Path: p.Path, InstalledSHA: p.SHA, FetchedAt: p.FetchedAt, Error: p.Error, Seeded: p.Seeded,
	}
}

func pluginFromRow(r PluginRow) Plugin {
	fetchedAt := r.FetchedAt
	if fetchedAt != nil {
		// Normalize to UTC so the wire JSON matches FSRegistry: the driver may scan the time back in the
		// connection's local zone.
		utc := fetchedAt.UTC()
		fetchedAt = &utc
	}
	return Plugin{
		Name: r.Name, Entry: r.Entry, Source: r.Source, Owner: r.Owner, Repo: r.Repo,
		Ref: r.Ref, Path: r.Path, SHA: r.InstalledSHA, FetchedAt: fetchedAt, Error: r.Error, Seeded: r.Seeded,
	}
}

// DBRegistry keeps rows in a sqlite or postgres table; clones still live on disk under root (CloneDir),
// which Delete removes.
type DBRegistry struct {
	db   *gorm.DB
	root string
	// ponytail: mu serializes writes in one process only; a second process sharing the DB can
	// race between the read and the write below. Use a DB lock if multi-process matters.
	mu sync.Mutex
}

// NewDBRegistry migrates PluginRow onto db and returns a registry backed by
// it. root is where clones live (Delete removes CloneDir(root, name)).
func NewDBRegistry(db *gorm.DB, root string) (*DBRegistry, error) {
	if err := db.AutoMigrate(&PluginRow{}); err != nil {
		return nil, err
	}
	return &DBRegistry{db: db, root: root}, nil
}

// List returns every row, sorted by name.
func (r *DBRegistry) List(ctx context.Context) ([]Plugin, error) {
	var rows []PluginRow
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	// Sort in Go: byte order like FSRegistry, independent of the DB collation.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	out := make([]Plugin, len(rows))
	for i, row := range rows {
		out[i] = pluginFromRow(row)
	}
	return out, nil
}

// putBusyRetries/putBusyRetryDelay absorb SQLITE_BUSY: with no row locking, two Put transactions can hit
// the database write lock despite busy_timeout. Vars so tests can shrink them.
var (
	putBusyRetries    = 20
	putBusyRetryDelay = 25 * time.Millisecond
)

func isSQLiteBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database is locked")
}

// Put mirrors FSRegistry.Put: same-identity replace, a different identity under the name is
// ErrNameCollision, and an unfetched re-Put keeps the existing sha/fetched_at.
func (r *DBRegistry) Put(ctx context.Context, p Plugin) error {
	if err := validName(p.Name); err != nil {
		return err
	}
	// r.mu only serializes THIS instance; two handles on one DB need the
	// transaction+row-lock below too, or their collision checks interleave.
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	for attempt := 0; attempt < putBusyRetries; attempt++ {
		err = r.putTx(ctx, p)
		if !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(putBusyRetryDelay)
	}
	return err
}

func (r *DBRegistry) putTx(ctx context.Context, p Plugin) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row PluginRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "name = ?", p.Name).Error
		found := true
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			found = false
		case err != nil:
			return err
		default:
			existing := pluginFromRow(row)
			if !SameIdentity(existing, p) {
				return fmt.Errorf("%w: plugin %q is already registered from %q, not %q", ErrNameCollision, p.Name, existing.Entry, p.Entry)
			}
			if p.SHA == "" && p.FetchedAt == nil {
				p.SHA, p.FetchedAt = existing.SHA, existing.FetchedAt
			}
		}
		newRow := rowFromPlugin(p)
		if found {
			// The row lock already proved SameIdentity; identityMatchClause's raw column comparison would wrongly
			// reject a caller passing blank owner/repo.
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "name"}},
				DoUpdates: clause.AssignmentColumns(pluginRowColumns),
			}).Create(&newRow).Error
		}
		// Nothing to lock, but a concurrent first Put of a different identity can race this INSERT;
		// identityMatchClause makes the loser's DO UPDATE a no-op.
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "name"}},
			DoUpdates: clause.AssignmentColumns(pluginRowColumns),
			Where:     clause.Where{Exprs: []clause.Expression{identityMatchClause}},
		}).Create(&newRow)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: plugin %q collided with a concurrently-inserted different entry", ErrNameCollision, p.Name)
		}
		return nil
	})
}

// pluginRowColumns is the upsert's DO UPDATE SET list - every PluginRow
// column but the primary key.
var pluginRowColumns = []string{"entry", "source", "owner", "repo", "ref", "path", "installed_sha", "fetched_at", "error", "seeded", "updated_at"}

// identityMatchClause is SameIdentity in SQL for the upsert's WHERE: github rows match by
// source+owner+repo, others by source+entry.
var identityMatchClause = clause.Expr{
	SQL:  "plugin_rows.source = excluded.source AND ((excluded.source = ? AND plugin_rows.owner = excluded.owner AND plugin_rows.repo = excluded.repo) OR (excluded.source <> ? AND plugin_rows.entry = excluded.entry))",
	Vars: []interface{}{SourceGitHub, SourceGitHub},
}

// Delete removes name's row and its clone dir under root. A name with no
// row is an error wrapping os.ErrNotExist, same as FSRegistry.
func (r *DBRegistry) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.db.WithContext(ctx).Where("name = ?", name).Delete(&PluginRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("plugin %q: %w", name, os.ErrNotExist)
	}
	return os.RemoveAll(filepath.Join(r.root, name))
}

// Fetch runs the free Fetch against r's root and persists the result
// regardless of success, so a failure is still visible via List.
func (r *DBRegistry) Fetch(ctx context.Context, p Plugin) (Plugin, error) {
	return fetchAndPut(ctx, r.root, r, p)
}

func (r *DBRegistry) CheckUpdate(ctx context.Context, p Plugin) (bool, string, error) {
	return CheckUpdate(ctx, p)
}
