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
	"encoding/json"
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
// ledger rebuild` and LoadEvents's from-scratch fallback. A node with BOTH a
// node.started and a terminal (done/failed) entry produces BOTH a
// node_start and a node_done/node_failed row (#1121 - StartedSeq and
// TerminalStatus are tracked independently in the fold for exactly this).
// Seq is each event's SOURCE ledger entry seq (the ledger's own lifetime seq
// space) - NOT comparable to the SSE table's per-run Seq (see LoadEvents's
// doc); a caller resuming a live SSE stream must treat this as a fresh
// reconstructed history, never as a continuation to filter by a table-space
// id. `quack ledger rebuild` (internal/cli/ledger.go) re-keys these by
// (node id, event name) to upsert against the real table instead of using
// this seq directly - see its doc for why.
func SynthesizeChatEvents(chatID string, res *fold.Result) []store.ChatEvent {
	type item struct {
		seq int64
		ev  stream.SSEEvent
	}
	var items []item
	for _, n := range res.Nodes {
		if n.StartedSeq > 0 {
			items = append(items, item{seq: n.StartedSeq, ev: stream.NodeStart(n.NodeID, "")})
		}
		switch n.TerminalStatus {
		case "done":
			items = append(items, item{seq: n.TerminalSeq, ev: stream.NodeDone(n.NodeID, stream.NodeDoneData{})})
		case "failed":
			items = append(items, item{seq: n.TerminalSeq, ev: stream.NodeFailed(n.NodeID, "")})
		}
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

// IsLifecycleEvent reports whether name is one this package can synthesize
// from the fold - node_start/node_done/node_failed only. Everything else
// (agent_token, agent_thinking, dag_plan, ...) is observational and has no
// WAL source (#1121 - rebuild must never treat those as candidates at all).
func IsLifecycleEvent(name string) bool {
	switch name {
	case stream.EventNodeStart, stream.EventNodeDone, stream.EventNodeFailed:
		return true
	}
	return false
}

// EventNodeID extracts the node_id a lifecycle SSEEvent carries, regardless
// of its concrete Data type (NodeStartData/NodeDoneData/NodeFailedData all
// share the same "node_id" JSON field) - used to key an EXISTING stored row
// the same way MissingLifecycleEvents keys a synthesized one, so the two
// sides of the match agree on identity without a shared concrete type.
func EventNodeID(ev stream.SSEEvent) (string, bool) {
	if !IsLifecycleEvent(ev.Name) {
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

// MissingLifecycleEvents returns the subset of SynthesizeChatEvents(res)
// for which have(nodeID, eventName) is false (#1121's non-destructive
// rebuild). An EXISTING lifecycle row is NEVER a candidate here, even if its
// content differs from the synthesized placeholder: the fold only ever
// carries node_id/turn/round, so a real row's richer fields (tokens, output,
// model, the real started_at) are always "different" from a reconstruction
// that never had them - overwriting on that basis would replace real data
// with a placeholder. Only a row that doesn't exist AT ALL is missing.
func MissingLifecycleEvents(chatID string, res *fold.Result, have func(nodeID, eventName string) bool) []store.ChatEvent {
	all := SynthesizeChatEvents(chatID, res)
	out := make([]store.ChatEvent, 0, len(all))
	for _, ce := range all {
		ev, err := UnmarshalEvent(ce.Event)
		if err != nil {
			continue
		}
		nodeID, ok := EventNodeID(ev)
		if !ok || have(nodeID, ev.Name) {
			continue
		}
		out = append(out, ce)
	}
	return out
}
