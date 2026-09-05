// Package ledger is quack's write-ahead log: one append-only, per-chat
// stream of typed Entry rows. Intents (artifact revisions, deliveries, node
// lifecycle) and observations (model/tool/agent calls, judge scores) share
// the same shape, the same seq space and the same reader API.
package ledger

import (
	"context"
	"time"
)

// SessionRef describes one recorded chat for a List call.
type SessionRef struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// LedgerStore is the WAL backend. Postgres is the only backend that gives
// the transactional, gapless seq the intent path needs; MemStore exists for
// tests. Never mutate or delete a single entry; Delete only drops a chat.
type LedgerStore interface {
	// AppendIntent allocates entry.Seq and writes entry atomically. A non-nil
	// error means nothing was written and the caller must not perform the
	// state change the entry describes.
	AppendIntent(ctx context.Context, entry Entry) (seq int64, err error)
	// ReadEntries returns every Entry for chatID with Seq >= fromSeq, in seq order.
	ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error)
	// List returns every chat with at least one entry.
	List(ctx context.Context) ([]SessionRef, error)
	// Delete removes a whole chat's entries (chat hard-delete only).
	Delete(ctx context.Context, sessionID string) error
}
