// This file is runlog's read of the ledger fold (V4 §4.9/#1101). The SSE
// table (store.ChatEvent, written by EventLog.Append) stays the source of
// truth for a live run's exact payloads - tokens, output, model - which the
// skinny node.*/judge.round WAL entries never carried. The fold only backs
// TWO paths that have no other source once the table is gone: Last-Event-ID
// resume when the table is empty but the WAL isn't (LoadEvents), and `quack
// ledger rebuild`'s regeneration of the table from the WAL alone. Both
// reconstructions are lossy by construction - see NodeState's doc.
package runlog

import (
	"context"
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
// when it has ANY rows for chatID, else - only when a WAL is armed -
// synthesized from the ledger fold. The fallback decision is deliberately
// NOT based on the fromSeq-filtered read: a caught-up client (table has
// rows, none newer than fromSeq) must get an empty result, not the whole
// reconstructed history resent - only a chat with literally zero table rows
// (e.g. GC'd, or never written) falls back. fromSeq is exclusive, matching
// store.LoadChatEvents's own "seq > afterSeq" contract; a synthesized event
// keeps its SOURCE ledger entry's seq as its id (never renumbered), so a
// fallback resume's ids stay comparable across calls and to a GC'd table's
// old ids.
func (l *EventLog) LoadEvents(ctx context.Context, chatID string, fromSeq int64) ([]store.ChatEvent, error) {
	exists, err := l.store.ChatEventsExist(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if exists || l.ledgerStore == nil {
		return l.store.LoadChatEvents(ctx, chatID, fromSeq)
	}
	res, ferr := fold.Fold(ctx, l.ledgerStore, chatID, 0)
	if ferr != nil || (len(res.Nodes) == 0 && len(res.JudgeRounds) == 0) {
		return nil, nil // no ledger data either; nothing to synthesize
	}
	all := SynthesizeChatEvents(chatID, res)
	out := make([]store.ChatEvent, 0, len(all))
	for _, e := range all {
		if e.Seq > fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// SynthesizeChatEvents turns a fold.Result into the ChatEvent rows a live
// run would have produced for its node lifecycle - the write side of `quack
// ledger rebuild` and LoadEvents's fallback. Seq is each event's SOURCE
// ledger entry seq (the ledger's own seq space), never renumbered - so
// LoadEvents's fromSeq filter and a resumed client's Last-Event-ID line up
// with what the ledger actually recorded.
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
