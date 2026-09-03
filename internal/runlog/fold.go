// This file is runlog's read of the ledger fold (V4 §4.9/#1101). The SSE
// table (store.ChatEvent, written by EventLog.Append) stays the source of
// truth for a live run's exact payloads - tokens, output, model - which the
// skinny node.*/judge.round WAL entries never carried. The fold only backs
// TWO paths that have no other source once the table is gone: a from-scratch
// Last-Event-ID resume (table empty, WAL armed, client's fromSeq == 0 - see
// LoadEvents's doc for why fromSeq > 0 is NOT served this way), and `quack
// ledger rebuild`'s regeneration of the table from the WAL alone. Both
// reconstructions are lossy by construction - see NodeState's doc.
package runlog

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// WithLedger arms l's fold fallback and returns l. store may be nil (no
// WAL) - LoadEvents then behaves exactly like the pre-#1101 direct table read.
func (l *EventLog) WithLedger(store ledger.LedgerStore) *EventLog {
	l.ledgerStore = store
	return l
}

// LoadEvents is Last-Event-ID resume's read path (V4 §4.9): the SSE table
// when it has ANY rows for chatID, else - only for a client with NO
// Last-Event-ID yet (fromSeq == 0) and a WAL armed - a full reconstruction
// from the ledger fold.
//
// The table's Seq and the ledger's Seq are two DIFFERENT numbering spaces
// and must never be compared: the table is per-RUN (Reset + NewPublisher
// restart it at 1 on every runChat/startNodeAsync/retryNodeAsync), while the
// ledger's Seq is per-CHAT LIFETIME and never resets. A client's
// Last-Event-ID is always in the table's per-run space, so a fromSeq > 0
// against the fold's lifetime seq would silently compare the wrong space -
// an arbitrary subset, not a correct resume. Since there is no honest way to
// map a per-run id onto the fold, a client that already has SOME progress
// (fromSeq > 0) gets an empty result on a lost table rather than a guess;
// only a client starting fresh (fromSeq == 0) - who has no prior numbering
// to preserve - gets the fold's reconstruction, itself numbered in the
// ledger's own seq space (a fresh id space from the client's point of view,
// since it has seen none yet).
func (l *EventLog) LoadEvents(ctx context.Context, chatID string, fromSeq int64) ([]store.ChatEvent, error) {
	exists, err := l.store.ChatEventsExist(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if exists || l.ledgerStore == nil || fromSeq != 0 {
		return l.store.LoadChatEvents(ctx, chatID, fromSeq)
	}
	res, ferr := fold.Fold(ctx, l.ledgerStore, chatID, 0)
	if ferr != nil {
		return nil, fmt.Errorf("runlog: fold chat %q for resume: %w", chatID, ferr)
	}
	if len(res.Nodes) == 0 && len(res.JudgeRounds) == 0 {
		return nil, nil // no ledger data either; nothing to synthesize
	}
	return SynthesizeChatEvents(chatID, res), nil
}

// SynthesizeChatEvents turns a fold.Result into the ChatEvent rows a live
// run would have produced for its node lifecycle - the write side of `quack
// ledger rebuild` and LoadEvents's from-scratch fallback. Seq is each
// event's SOURCE ledger entry seq (the ledger's own lifetime seq space) -
// NOT comparable to the SSE table's per-run Seq (see LoadEvents's doc); a
// caller resuming a live SSE stream must treat this as a fresh reconstructed
// history, never as a continuation to filter by a table-space id.
func SynthesizeChatEvents(chatID string, res *fold.Result) []store.ChatEvent {
	type item struct {
		seq int64
		ev  stream.SSEEvent
	}
	var items []item
	for _, n := range res.Nodes {
		var ev stream.SSEEvent
		switch n.Status {
		case "started":
			ev = stream.NodeStart(n.NodeID, "")
		case "done":
			ev = stream.NodeDone(n.NodeID, stream.NodeDoneData{})
		case "failed":
			ev = stream.NodeFailed(n.NodeID, "")
		default:
			continue
		}
		items = append(items, item{seq: n.Seq, ev: ev})
	}
	// judge.round entries carry no dedicated SSE event yet (design V4 §5's
	// stream/event.go step is a later P) - out of #1101's scope; the fold
	// keeps them (Result.JudgeRounds) for `ledger show`/rebuild's artifact
	// side, just not as a synthesized SSE event here.
	sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	now := time.Now().UTC()
	out := make([]store.ChatEvent, 0, len(items))
	for _, it := range items {
		js, err := MarshalEvent(it.ev)
		if err != nil {
			continue
		}
		out = append(out, store.ChatEvent{ChatID: chatID, Seq: it.seq, Event: js, CreatedAt: now})
	}
	return out
}
