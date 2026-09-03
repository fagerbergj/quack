// This file adds a second Session constructor alongside Load's bundle-file
// path (V4 §4.9/#1101): fold a chat's ledger to seq N instead of
// sequence-matching a recorded bundle, for a caller that wants to continue
// LIVE from that boundary (EnableFork) rather than replay a whole recording.
// The existing bundle path is unchanged.
package replay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fagerbergj/quack/internal/ledger"
)

// otelEntryKind mirrors ledger.otelEntryKind (unexported there): the kind
// tag on a raw OTel exporter line, the same JSON shape loadJSONL parses from
// a bundle file's entries.jsonl.
const otelEntryKind = "otel"

// FoldToSeq builds a Session from chatID's ledger entries up to and
// including seqN, instead of a bundle file. Streams built from entries
// after seqN are simply absent - a caller that then calls EnableFork
// resumes live from exactly that boundary, with no bundle export/download
// round-trip needed.
func FoldToSeq(ctx context.Context, store ledger.LedgerStore, chatID string, seqN int64) (*Session, error) {
	entries, err := store.ReadEntries(ctx, chatID, 0)
	if err != nil {
		return nil, fmt.Errorf("replay: read ledger entries for chat %q: %w", chatID, err)
	}
	s := &Session{streams: map[StreamKey]*streamState{}}
	for _, e := range entries {
		if e.Kind != otelEntryKind || e.Seq > seqN {
			continue
		}
		var l line
		if err := json.Unmarshal(e.Payload, &l); err != nil {
			continue // matches buildSession's tolerance for a non-line entry
		}
		s.ingest(l)
	}
	s.finalize()
	return s, nil
}
