// Package docstore is the document record store: the source-of-truth persistence
// for ingested documents (title, cleaned text, classification, provenance). It is
// a port (DocStore) plus a thin reference Postgres adapter, selected by a `kind`
// factory like internal/memory and the tool backends — so an opinionated or
// out-of-process document store can replace it without touching callers.
//
// The reference adapter's schema is deliberately minimal: only the fields quack
// needs, not document-pipeline's jobs/artifacts/contexts model. Adopters who want
// a richer store bring their own adapter behind this same port.
package docstore

import (
	"context"
	"fmt"
	"time"
)

// Document is one ingested document record. Tags is stored as JSON. Content is
// the cleaned/clarified text; the chunked vectors live in the vector store and
// the full-text index is derived from this record — neither is duplicated here.
type Document struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	ContentHash string    `gorm:"uniqueIndex" json:"content_hash"` // SHA-256 of source bytes; dedup key
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Summary     string    `json:"summary"`
	Tags        []string  `gorm:"serializer:json" json:"tags"`
	Series      string    `gorm:"index" json:"series,omitempty"`
	DateMonth   string    `json:"date_month,omitempty"`
	SourceRef   string    `json:"source_ref,omitempty"` // blob-store ref to the original media
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DocStore persists and retrieves document records. The reference implementation
// is Postgres (newPostgres); a custom store implements this interface instead.
type DocStore interface {
	Create(ctx context.Context, d Document) error
	Get(ctx context.Context, id string) (Document, bool, error)
	GetByHash(ctx context.Context, contentHash string) (Document, bool, error)
	Update(ctx context.Context, d Document) error
}

// New selects the document-store adapter for kind (default: postgres) and opens
// it. Empty url is an error (unlike the vector store, document records have no
// self-disable — if documents are configured, they must persist somewhere).
func New(kind, url string) (DocStore, error) {
	if kind == "" {
		kind = "postgres"
	}
	switch kind {
	case "postgres":
		return newPostgres(url)
	default:
		return nil, fmt.Errorf("docstore: unsupported kind %q", kind)
	}
}
