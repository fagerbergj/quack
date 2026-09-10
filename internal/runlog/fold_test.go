package runlog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/fold"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// mustSeedChat upserts a bare chats row - chat_events/dag_plans now FK to
// chats.id (#1296), so a fold/table-write test needs one before it can insert.
func mustSeedChat(t *testing.T, st *store.Store, chatID string) {
	t.Helper()
	if err := st.SetChatOrigin(context.Background(), chatID, "", ""); err != nil {
		t.Fatalf("seed chat %s: %v", chatID, err)
	}
}

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
// resumes from the fold instead of returning nothing - BOTH the node_start
// and node_done rows (#1121: start and terminal are synthesized independently).
func TestLoadEvents_FallsBackToFold(t *testing.T) {
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	const chatID = "chat-1"
	mustSeedChat(t, st, chatID)
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted)
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeDone)

	l := NewEventLog(st).WithLedger(ls)
	evs, err := l.LoadEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("LoadEvents returned %d events, want 2 (node_start + node_done)", len(evs))
	}
	names := make([]string, len(evs))
	for i, e := range evs {
		ev, err := UnmarshalEvent(e.Event)
		if err != nil {
			t.Fatalf("UnmarshalEvent: %v", err)
		}
		names[i] = ev.Name
	}
	if names[0] != "node_start" || names[1] != "node_done" {
		t.Fatalf("event names = %v, want [node_start node_done] in seq order", names)
	}
}

// TestLoadEvents_PrefersTable: a non-empty SSE table wins over the fold,
// even with a WAL armed - unchanged behavior when the table already has rows.
func TestLoadEvents_PrefersTable(t *testing.T) {
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	const chatID = "chat-1"
	mustSeedChat(t, st, chatID)
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

// TestLoadEvents_NeverFoldsWithPriorProgress: the table's Seq (per-RUN) and the
// ledger's Seq (per-CHAT LIFETIME) are different numbering spaces - a client with
// fromSeq > 0 already holds a per-run id, which the fold cannot honestly satisfy. A lost table must return empty for such a client, never a fold-derived guess in the wrong space.
func TestLoadEvents_NeverFoldsWithPriorProgress(t *testing.T) {
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
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
	ls := ledgertest.NewMemStore()
	const chatID = "chat-1"
	mustSeedChat(t, st, chatID)
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

// TestLoadEvents_CrashBetweenIntentAndWatermark is #1144 P3's kill-9 test: the
// ledger already has durable node.* intents (as if a live writer had appended
// them) but the process died before this chat's "sse" projection ever wrote a single row or watermark - simulating a kill -9 right after the WAL append. A brand-new EventLog (a fresh boot's process) must resume by folding the WHOLE chat from watermark 0 and land on exactly what an independent fold.Fold computes - no CLI, no rebuild command involved. A second resume on the same (now-populated) table must not re-insert or duplicate anything, proving the watermark advance actually stuck.
func TestLoadEvents_CrashBetweenIntentAndWatermark(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	const chatID = "chat-crash"
	mustSeedChat(t, st, chatID)

	// The "crash": these intents are durably in the ledger, but nothing ever
	// wrote to chat_events or projection_watermarks for this chat - as if the
	// process died between the ledger append and the first SSE fold+write.
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeStarted)
	appendNode(t, ls, chatID, "n1", "t1", ledger.KindNodeDone)
	appendNode(t, ls, chatID, "n2", "t1", ledger.KindNodeStarted)

	want, err := fold.Fold(ctx, ls, chatID, 0)
	if err != nil {
		t.Fatalf("independent fold.Fold: %v", err)
	}
	wantEvents := SynthesizeChatEvents(chatID, want)

	// "Restart": a fresh EventLog against the same durable store/ledger, the
	// way boot re-wires runlog.NewEventLog(st).WithLedger(ls) from scratch.
	l := NewEventLog(st).WithLedger(ls)
	got, err := l.LoadEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents after crash: %v", err)
	}
	if len(got) != len(wantEvents) {
		t.Fatalf("resumed %d events, want %d (independent fold): got=%+v want=%+v", len(got), len(wantEvents), got, wantEvents)
	}
	// Compare node id + event name, not the raw JSON: LoadEvents' fold runs
	// independently of the `want` fold above, and node.* payloads carry no
	// content beyond node id (Agent/Output are populated by the live path,
	// not reconstructed) - the two folds' events differ in nothing else.
	for i := range got {
		gotEv, err := UnmarshalEvent(got[i].Event)
		if err != nil {
			t.Fatalf("UnmarshalEvent got[%d]: %v", i, err)
		}
		wantEv, err := UnmarshalEvent(wantEvents[i].Event)
		if err != nil {
			t.Fatalf("UnmarshalEvent want[%d]: %v", i, err)
		}
		gotID, _ := EventNodeID(gotEv)
		wantID, _ := EventNodeID(wantEv)
		if gotEv.Name != wantEv.Name || gotID != wantID {
			t.Fatalf("event %d = (%s, %s), want (%s, %s)", i, gotEv.Name, gotID, wantEv.Name, wantID)
		}
	}

	wm, err := st.GetProjectionWatermark(ctx, chatID, sseProjection)
	if err != nil {
		t.Fatalf("GetProjectionWatermark: %v", err)
	}
	if wm != want.LastSeq {
		t.Fatalf("watermark = %d after resume, want %d (the fold's LastSeq)", wm, want.LastSeq)
	}

	// A second resume must not duplicate: the table now has rows, so
	// LoadEvents serves it directly and never re-folds or re-inserts.
	again, err := l.LoadEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents second resume: %v", err)
	}
	if len(again) != len(wantEvents) {
		t.Fatalf("second resume returned %d events, want %d (no duplicates)", len(again), len(wantEvents))
	}
}

// TestSynthesizeChatEvents_UsesSourceEntryTimestamps pins the fold-
// reconstruction fix: a folded node_start/node_done must carry the ORIGINAL
// ledger entry's At, not the wall-clock moment of reconstruction - otherwise
// a resumed/rebuilt chat renders a started/finished_at_ms minutes (or
// rebuild-runs later than the entry, days) after the real run.
func TestSynthesizeChatEvents_UsesSourceEntryTimestamps(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	const chatID = "chat-ts"
	started := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)

	payload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
	}{NodeID: "n1", Turn: "t1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload, At: started}); err != nil {
		t.Fatalf("append started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload, At: finished}); err != nil {
		t.Fatalf("append done: %v", err)
	}

	res, err := fold.Fold(ctx, ls, chatID, 0)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	// Reconstruction happens well after the original run - proves the
	// synthesized timestamps come from `res`, not from calling this "now".
	time.Sleep(10 * time.Millisecond)
	events := SynthesizeChatEvents(chatID, res)

	var gotStart, gotDone bool
	for _, ce := range events {
		ev, err := UnmarshalEvent(ce.Event)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		raw, ok := ev.Data.(json.RawMessage)
		if !ok {
			t.Fatalf("event %q: Data = %T, want json.RawMessage", ev.Name, ev.Data)
		}
		switch ev.Name {
		case stream.EventNodeStart:
			var d stream.NodeStartData
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatalf("unmarshal node_start: %v", err)
			}
			gotStart = true
			if d.StartedAtMs != started.UnixMilli() {
				t.Errorf("StartedAtMs = %d, want %d (the original entry's At)", d.StartedAtMs, started.UnixMilli())
			}
		case stream.EventNodeDone:
			var d stream.NodeDoneData
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatalf("unmarshal node_done: %v", err)
			}
			gotDone = true
			if d.FinishedAtMs != finished.UnixMilli() {
				t.Errorf("FinishedAtMs = %d, want %d (the original entry's At)", d.FinishedAtMs, finished.UnixMilli())
			}
		}
	}
	if !gotStart || !gotDone {
		t.Fatalf("events = %+v, want both node_start and node_done", events)
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
