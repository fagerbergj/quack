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

// TestLoadEvents_FallbackHonorsFromSeq: the fold fallback must not resend
// the whole reconstructed history - only entries newer than fromSeq (the
// SOURCE ledger entry's own seq, never renumbered) come back.
func TestLoadEvents_FallbackHonorsFromSeq(t *testing.T) {
	st := newTestStore(t)
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	seq1 := appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted) // final state: started
	seq2 := appendNode(t, ls, chatID, "n2", "t2", ledger.KindNodeStarted) // final state: started

	l := NewEventLog(st).WithLedger(ls)
	evs, err := l.LoadEvents(context.Background(), chatID, seq1)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("LoadEvents returned %d events, want 1 (only n2, seq %d > fromSeq %d)", len(evs), seq2, seq1)
	}
	if evs[0].Seq != seq2 {
		t.Fatalf("event seq = %d, want the source ledger entry's own seq %d", evs[0].Seq, seq2)
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
