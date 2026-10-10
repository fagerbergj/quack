// The SSE table stays the source of truth for a live run's payloads; this fold of the skinny node.* WAL entries
// backs only a fresh (fromSeq == 0) resume on a lost table and `quack ledger rebuild`. Both are lossy.
package runlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// sseProjection names the "sse" row in projection_watermarks.
const sseProjection = "sse"

// WithLedger arms l's fold fallback and returns l. A nil store (no WAL) makes LoadEvents a plain table read.
func (l *EventLog) WithLedger(store ledger.LedgerStore) *EventLog {
	l.ledgerStore = store
	return l
}

// LoadEvents reads the SSE table, else (fromSeq == 0, WAL armed) folds the ledger from the "sse" watermark.
// Table seq is per-run, ledger seq per-chat: they never compare, so a fromSeq > 0 client gets empty, not a guess.
func (l *EventLog) LoadEvents(ctx context.Context, chatID string, fromSeq int64) ([]store.ChatEvent, error) {
	exists, err := l.store.ChatEventsExist(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if exists || l.ledgerStore == nil || fromSeq != 0 {
		return l.store.LoadChatEvents(ctx, chatID, fromSeq)
	}
	events, _, ferr := l.foldSSEFromWatermark(ctx, chatID)
	if ferr != nil {
		return nil, fmt.Errorf("runlog: fold chat %q for resume: %w", chatID, ferr)
	}
	return events, nil
}

// foldSSEFromWatermark folds from the "sse" watermark and writes the rows and new watermark in one transaction,
// so a crash can never leave the projection ahead of what it wrote.
func (l *EventLog) foldSSEFromWatermark(ctx context.Context, chatID string) ([]store.ChatEvent, int64, error) {
	watermark, err := l.store.GetProjectionWatermark(ctx, chatID, sseProjection)
	if err != nil {
		return nil, 0, err
	}
	res, err := fold.Apply(ctx, l.ledgerStore, chatID, watermark)
	if err != nil {
		return nil, 0, err
	}
	if res.LastSeq <= watermark {
		return nil, watermark, nil // nothing new since the last fold
	}
	events := SynthesizeChatEvents(chatID, res)
	err = l.store.InTx(ctx, func(tx *gorm.DB) error {
		for _, ev := range events {
			if werr := store.InsertChatEventTx(tx, ev); werr != nil {
				return werr
			}
		}
		return store.SetProjectionWatermarkTx(tx, chatID, sseProjection, res.LastSeq)
	})
	if err != nil {
		return nil, 0, err
	}
	return events, res.LastSeq, nil
}

// SynthesizeChatEvents rebuilds a node's lifecycle rows (node_start and terminal independently). Seq is the
// source ledger seq, not the SSE table's per-run seq: treat the result as fresh history, never a continuation.
func SynthesizeChatEvents(chatID string, res *fold.Result) []store.ChatEvent {
	type item struct {
		seq int64
		ev  stream.SSEEvent
	}
	var items []item
	for _, n := range res.Nodes {
		// Built directly, not via stream.NodeStart/NodeDone: those stamp time.Now(), and a rebuilt row must
		// carry the source entry's At or the chat shows reconstruction time.
		if n.StartedSeq > 0 {
			items = append(items, item{seq: n.StartedSeq, ev: stream.SSEEvent{Name: stream.EventNodeStart, Data: stream.NodeStartData{
				NodeID: n.NodeID, StartedAtMs: n.StartedAt.UnixMilli(),
			}}})
		}
		switch n.TerminalStatus {
		case "done":
			items = append(items, item{seq: n.TerminalSeq, ev: stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{
				NodeID: n.NodeID, FinishedAtMs: n.TerminalAt.UnixMilli(),
			}}})
		case "failed":
			items = append(items, item{seq: n.TerminalSeq, ev: stream.SSEEvent{Name: stream.EventNodeFailed, Data: stream.NodeFailedData{
				NodeID: n.NodeID, FinishedAtMs: n.TerminalAt.UnixMilli(),
			}}})
		case "cancelled":
			items = append(items, item{seq: n.TerminalSeq, ev: stream.SSEEvent{Name: stream.EventNodeCancelled, Data: stream.NodeCancelledData{
				NodeID: n.NodeID, FinishedAtMs: n.TerminalAt.UnixMilli(),
			}}})
		}
	}
	// judge_round artifact.revision entries have no SSE event; the fold keeps them in Result.JudgeRounds.
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

// isLifecycleEvent reports node_start/node_done/node_failed, the only events with a WAL source;
// rebuild must never treat observational events as candidates.
func isLifecycleEvent(name string) bool {
	switch name {
	case stream.EventNodeStart, stream.EventNodeDone, stream.EventNodeFailed:
		return true
	}
	return false
}

// EventNodeID extracts a lifecycle event's node_id whatever its concrete Data type.
func EventNodeID(ev stream.SSEEvent) (string, bool) {
	if !isLifecycleEvent(ev.Name) {
		return "", false
	}
	var d struct {
		NodeID string `json:"node_id"`
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return "", false
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.NodeID == "" {
		return "", false
	}
	return d.NodeID, true
}
