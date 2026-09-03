package runlog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

func appendNode(t *testing.T, s ledger.LedgerStore, chatID, nodeID, turn, kind string) int64 {
	t.Helper()
	payload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
		Round  int    `json:"round"`
	}{NodeID: nodeID, Turn: turn})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	seq, err := s.AppendIntent(context.Background(), ledger.Entry{ChatID: chatID, Kind: kind, Payload: payload})
	if err != nil {
		t.Fatalf("AppendIntent %s: %v", kind, err)
	}
	return seq
}

// TestLoadEvents_FallsBackToFold: an empty SSE table with a WAL armed
// resumes from the fold instead of returning nothing.
func TestLoadEvents_FallsBackToFold(t *testing.T) {
	st := newTestStore(t)
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted)
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeDone)

	l := NewEventLog(st).WithLedger(ls)
	evs, err := l.LoadEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("LoadEvents returned %d events, want 1 (the node's final state)", len(evs))
	}
	ev, err := UnmarshalEvent(evs[0].Event)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	if ev.Name != "node_done" {
		t.Fatalf("event name = %q, want node_done", ev.Name)
	}
}

// TestLoadEvents_PrefersTable: a non-empty SSE table wins over the fold,
// even with a WAL armed - unchanged behavior when the table already has rows.
func TestLoadEvents_PrefersTable(t *testing.T) {
	st := newTestStore(t)
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted) // ledger disagrees with the table on purpose
	l := NewEventLog(st).WithLedger(ls)

	js, err := MarshalEvent(stream.NodeStart("n1", "worker"))
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if err := st.InsertChatEvent(context.Background(), store.ChatEvent{ChatID: chatID, Seq: 1, Event: js, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed table: %v", err)
	}

	evs, err := l.LoadEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("LoadEvents returned %d events, want the 1 table row", len(evs))
	}
}

// TestLoadEvents_NeverFoldsWithPriorProgress: the table's Seq (per-RUN) and
// the ledger's Seq (per-CHAT LIFETIME) are different numbering spaces - a
// client with fromSeq > 0 already holds a per-run id, which the fold cannot
// honestly satisfy. A lost table must return empty for such a client, never
// a fold-derived guess in the wrong space.
func TestLoadEvents_NeverFoldsWithPriorProgress(t *testing.T) {
	st := newTestStore(t)
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted) // WAL has data; must still be ignored

	l := NewEventLog(st).WithLedger(ls)
	evs, err := l.LoadEvents(context.Background(), chatID, 3) // client already has SOME progress
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("LoadEvents returned %d events for fromSeq>0 against a lost table, want 0 (unmappable, not a guess)", len(evs))
	}
}

// TestLoadEvents_CaughtUpClientNeverFolds: a table that already has rows,
// none newer than fromSeq, is "caught up" - it must return empty, NOT fall
// back to the fold and resend reconstructed history.
func TestLoadEvents_CaughtUpClientNeverFolds(t *testing.T) {
	st := newTestStore(t)
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted) // ledger has data; must be ignored

	js, err := MarshalEvent(stream.NodeStart("n1", "worker"))
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if err := st.InsertChatEvent(context.Background(), store.ChatEvent{ChatID: chatID, Seq: 5, Event: js, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed table: %v", err)
	}

	l := NewEventLog(st).WithLedger(ls)
	evs, err := l.LoadEvents(context.Background(), chatID, 5) // caught up: has seen seq 5
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("LoadEvents returned %d events for a caught-up client, want 0", len(evs))
	}
}

// TestLoadEvents_NoWAL: nil ledgerStore behaves exactly like a direct table read.
func TestLoadEvents_NoWAL(t *testing.T) {
	st := newTestStore(t)
	l := NewEventLog(st)
	evs, err := l.LoadEvents(context.Background(), "chat-1", 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("LoadEvents = %d events, want 0 (no table rows, no WAL)", len(evs))
	}
}
