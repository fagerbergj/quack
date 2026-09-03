// Package ledger is the replay ledger's storage seam: an append-only,
// per-session event log written by the OTel logger provider's in-process
// exporter (internal/otelobs) and read back by export/replay (a later
// milestone). See .quack/replay-log.md for the full design.
package ledger

import (
	"context"
	"io"
	"time"
)

// SessionRef describes one recorded session for a List call.
type SessionRef struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// LedgerStore is the append-only ledger backend, named like every other
// backend in the stores registry (config.StoreConfig). The seam is
// stream-level, not file-level, so a future object-storage adapter (no
// native append on S3) can batch entries into chunk objects and stitch them
// on ReadStream - callers never see that distinction.
//
// Never mutate or delete a single entry; Delete only ever drops a whole
// session.
type LedgerStore interface {
	// Append adds entry as the next record for sessionID, in call order.
	Append(ctx context.Context, sessionID string, entry []byte) error
	// ReadStream returns every entry for sessionID, in append order.
	ReadStream(ctx context.Context, sessionID string) (io.ReadCloser, error)
	// List returns every recorded session.
	List(ctx context.Context) ([]SessionRef, error)
	// Delete removes a whole session's recording (GC only - never a partial entry).
	Delete(ctx context.Context, sessionID string) error

	// AppendIntent is the WAL's fail-closed path (V4 §4.8): it allocates
	// entry.Seq and writes entry as one atomic operation, returning the
	// allocated seq. A non-nil error means nothing was written, and the
	// caller must not perform the state change the entry describes either.
	// The FS store cannot make this transactional against its JSONL file;
	// it appends best-effort and documents the gap on its own method.
	AppendIntent(ctx context.Context, entry Entry) (seq int64, err error)
	// ReadEntries returns every Entry for chatID with Seq >= fromSeq, in
	// seq order - the reader side of the WAL, used by folds/projections.
	ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error)
}
