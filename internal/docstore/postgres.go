package docstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// pgStore is the reference Postgres adapter for DocStore. It opens its own GORM
// connection (the document store is a distinct, swappable store, possibly a
// different database than sessions) and auto-migrates the minimal documents table.
type pgStore struct {
	db *gorm.DB
}

func newPostgres(dsn string) (*pgStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("docstore: postgres url is empty")
	}
	gormCfg := &gorm.Config{Logger: logger.New(
		slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		logger.Config{SlowThreshold: 200 * time.Millisecond, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true},
	)}
	db, err := gorm.Open(postgres.Open(dsn), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("docstore: open postgres: %w", err)
	}
	if err := db.AutoMigrate(&Document{}); err != nil {
		return nil, fmt.Errorf("docstore: migrate: %w", err)
	}
	return &pgStore{db: db}, nil
}

func (s *pgStore) Create(ctx context.Context, d Document) error {
	if err := s.db.WithContext(ctx).Create(&d).Error; err != nil {
		return fmt.Errorf("docstore: create %q: %w", d.ID, err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, id string) (Document, bool, error) {
	return s.first(ctx, "id = ?", id)
}

func (s *pgStore) GetByHash(ctx context.Context, contentHash string) (Document, bool, error) {
	return s.first(ctx, "content_hash = ?", contentHash)
}

// first runs a single-row lookup, mapping "not found" to (zero, false, nil) so
// callers branch on the bool rather than on a sentinel error.
func (s *pgStore) first(ctx context.Context, query string, arg any) (Document, bool, error) {
	var d Document
	err := s.db.WithContext(ctx).Where(query, arg).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("docstore: lookup: %w", err)
	}
	return d, true, nil
}

func (s *pgStore) Update(ctx context.Context, d Document) error {
	// Save writes all fields (full replace of the record by primary key).
	if err := s.db.WithContext(ctx).Save(&d).Error; err != nil {
		return fmt.Errorf("docstore: update %q: %w", d.ID, err)
	}
	return nil
}
