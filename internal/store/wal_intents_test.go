package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestSaveTurn_CrashBetweenIntentAndRow is #1144 P5's required kill-9 test:
// the process appends turn.created and dies before the ChatTurn row lands
// (SaveTurn's real path is AppendIntent-then-Create, so this is the
// documented gap between them, not a hypothetical). No CLI/recovery
// machinery is wired for this new writer family (out of scope for P5 - see
// the PR body); this proves the WAL invariant recovery would rely on: the
// intent alone carries everything SaveTurn's row would have, so replaying
// it reaches the identical row.
func TestSaveTurn_CrashBetweenIntentAndRow(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledger.NewMemStore()
	st.SetWALLedger(ls)

	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Simulate the crash directly, the same way TestRecover_CrashBetweenIntentAndRow
	// does for artifact revisions: append the intent SaveTurn would have
	// appended, then never call the row write it precedes.
	payload, err := json.Marshal(struct {
		UserText string `json:"user_text"`
		Seq      int    `json:"seq"`
	}{UserText: "hello", Seq: 0})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{
		ChatID: chat.ID, TurnID: "turn-1", Kind: ledger.KindTurnCreated, Key: "turn-1", Payload: payload,
	}); err != nil {
		t.Fatalf("AppendIntent turn.created: %v", err)
	}

	// The crash: no ChatTurn row exists yet.
	turns, err := st.ListTurns(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("ListTurns = %d, want 0 (row must not exist before recovery replays the intent)", len(turns))
	}

	// Recovery replay: decode the surviving intent and complete the write it
	// described - no CLI involved, exactly what a boot-time recoverer would do.
	entries, err := ls.ReadEntries(ctx, chat.ID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var found ledger.Entry
	for _, e := range entries {
		if e.Kind == ledger.KindTurnCreated {
			found = e
		}
	}
	if found.Kind == "" {
		t.Fatalf("turn.created intent did not survive the simulated crash")
	}
	var p struct {
		UserText string `json:"user_text"`
	}
	if err := json.Unmarshal(found.Payload, &p); err != nil {
		t.Fatalf("unmarshal surviving intent: %v", err)
	}
	if err := st.db.Create(&ChatTurn{ID: found.TurnID, ChatID: found.ChatID, UserText: p.UserText}).Error; err != nil {
		t.Fatalf("replay row write: %v", err)
	}

	turns, err = st.ListTurns(ctx, chat.ID)
	if err != nil {
		t.Fatalf("ListTurns after replay: %v", err)
	}
	if len(turns) != 1 || turns[0].UserText != "hello" {
		t.Fatalf("ListTurns after replay = %+v, want one turn with UserText=hello", turns)
	}
}

// TestCreateChat_LedgerAppendFailureBlocksRow proves the fail-closed half of
// the same invariant from the other direction: if the intent never lands,
// the row must never land either - the ordering that makes "row without a
// matching intent" impossible.
func TestCreateChat_LedgerAppendFailureBlocksRow(t *testing.T) {
	st := newTestStore(t)
	st.SetWALLedger(failingLedger{})

	if _, err := st.CreateChat(context.Background(), "sys"); err == nil {
		t.Fatal("CreateChat: want error when the WAL append fails, got nil")
	}

	var count int64
	if err := st.db.Model(&Chat{}).Count(&count).Error; err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if count != 0 {
		t.Fatalf("chats = %d, want 0 (a failed intent must block the row)", count)
	}
}

// failingLedger fails every AppendIntent - fail-closed's other half.
type failingLedger struct{ ledger.LedgerStore }

func (failingLedger) AppendIntent(context.Context, ledger.Entry) (int64, error) {
	return 0, errors.New("ledger: unreachable")
}

// TestSaveDagPlan_ResumeIsWALIdempotent proves the boot-resume re-yield case
// (SaveDagPlan's doc: "a boot resume re-yields the same stashed plan") never
// grows a duplicate plan.saved entry - the WAL append shares the same
// skip-if-exists behavior the DB row already had.
func TestSaveDagPlan_ResumeIsWALIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledger.NewMemStore()
	st.SetWALLedger(ls)

	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveTurn(ctx, chat.ID, "turn-1", "hi"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.SaveDagPlan(ctx, chat.ID, "plan-1", "turn-1", `{"nodes":[]}`); err != nil {
			t.Fatalf("SaveDagPlan resume %d: %v", i, err)
		}
	}
	entries, err := ls.ReadEntries(ctx, chat.ID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var planSaved int
	for _, e := range entries {
		if e.Kind == ledger.KindPlanSaved {
			planSaved++
		}
	}
	if planSaved != 1 {
		t.Fatalf("plan.saved entries = %d, want 1 (three resumes must not append three times)", planSaved)
	}
}
