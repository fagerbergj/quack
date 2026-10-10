package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

// The process appends turn.created and dies before the ChatTurn row lands. No recovery is wired for this
// writer; this proves the intent alone carries everything the row would, so replaying it reaches the same row.
func TestSaveTurn_CrashBetweenIntentAndRow(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	st.SetWALLedger(ls)

	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// Simulate the crash: append the intent SaveTurn would have, then never write the row.
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

// Fail-closed: if the intent never lands, the row never lands, so a row without an intent is impossible.
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

// A boot resume re-yields the same stashed plan; the WAL append must skip-if-exists like the DB row does.
func TestSaveDagPlan_ResumeIsWALIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
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

// TestSaveDagPlan_ExtensionRecordsItsTurn: a plan grown by edit_plan in a later turn keeps one row on
// its first turn, with the grown plan_json and the extending turn as the one a retry answers.
func TestSaveDagPlan_ExtensionRecordsItsTurn(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	st.SetWALLedger(ls)
	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []string{"turn-1", "turn-2"} {
		if err := st.SaveTurn(ctx, chat.ID, turn, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveDagPlan(ctx, chat.ID, "plan-1", "turn-1", `{"nodes":["r1"]}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDagPlan(ctx, chat.ID, "plan-1", "turn-2", `{"nodes":["r1","r3"]}`); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetLatestDagPlan(ctx, chat.ID)
	if err != nil || p == nil || p.TurnID != "turn-1" || p.RunTurnID() != "turn-2" || p.PlanJSON != `{"nodes":["r1","r3"]}` {
		t.Fatalf("plan = %+v (err %v), want the grown plan, still turn-1's, run by turn-2", p, err)
	}
	entries, _ := ls.ReadEntries(ctx, chat.ID, 0)
	saved := 0
	for _, e := range entries {
		if e.Kind == ledger.KindPlanSaved {
			saved++
		}
	}
	if saved != 2 {
		t.Errorf("plan.saved entries = %d, want one per distinct save", saved)
	}
}

// The checkpoint is one row replaced in place, and a second WriteCheckpoint must consume it as its seed
// (passing the seed's own LastSeq as ApplySeeded's `from` silently skips seeding).
func TestWriteCheckpoint_UpsertsOneRowAndSeedsNextFold(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ls := ledgertest.NewMemStore()
	st.SetWALLedger(ls)

	chat, err := st.CreateChat(ctx, "sys")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	appendRev := func(rev, parent int) {
		t.Helper()
		payload, merr := json.Marshal(struct {
			Revision       int `json:"revision"`
			ParentRevision int `json:"parent_revision"`
		}{rev, parent})
		if merr != nil {
			t.Fatalf("marshal: %v", merr)
		}
		if _, aerr := ls.AppendIntent(ctx, ledger.Entry{
			ChatID: chat.ID, Kind: ledger.KindArtifactRevision, Key: "art1", Payload: payload,
		}); aerr != nil {
			t.Fatalf("AppendIntent revision: %v", aerr)
		}
	}

	appendRev(1, 0)
	if err := st.WriteCheckpoint(ctx, chat.ID); err != nil {
		t.Fatalf("WriteCheckpoint 1: %v", err)
	}
	var count int64
	if err := st.db.Model(&Checkpoint{}).Where("chat_id = ?", chat.ID).Count(&count).Error; err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if count != 1 {
		t.Fatalf("checkpoint rows = %d, want 1", count)
	}

	appendRev(2, 1)
	if err := st.WriteCheckpoint(ctx, chat.ID); err != nil {
		t.Fatalf("WriteCheckpoint 2: %v", err)
	}
	if err := st.db.Model(&Checkpoint{}).Where("chat_id = ?", chat.ID).Count(&count).Error; err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if count != 1 {
		t.Fatalf("checkpoint rows after second write = %d, want 1 (replaced, not appended)", count)
	}

	seed := st.loadCheckpointSeed(ctx, chat.ID)
	if seed == nil {
		t.Fatal("loadCheckpointSeed: nil, want the row just written")
	}
	latest, ok := seed.Artifacts["art1"].Latest()
	if !ok || latest.Revision != 2 {
		t.Fatalf("checkpoint seed latest = %+v, ok=%v, want revision 2", latest, ok)
	}
	if len(seed.Artifacts["art1"].Revisions) != 2 {
		t.Fatalf("checkpoint seed revisions = %d, want 2 (must actually include both, not just the delta)", len(seed.Artifacts["art1"].Revisions))
	}
}
