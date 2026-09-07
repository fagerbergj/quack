package fold

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

func newMemStore(t *testing.T) *ledger.MemStore {
	t.Helper()
	return ledger.NewMemStore()
}

func appendRevision(t *testing.T, s ledger.LedgerStore, chatID, id string, revision, parent int) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Revision       int `json:"revision"`
		ParentRevision int `json:"parent_revision"`
	}{Revision: revision, ParentRevision: parent})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, Kind: ledger.KindArtifactRevision, Key: id, Payload: payload,
	}); err != nil {
		t.Fatalf("AppendIntent revision: %v", err)
	}
}

func appendAborted(t *testing.T, s ledger.LedgerStore, chatID, id string, revision int) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Revision int `json:"revision"`
	}{Revision: revision})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, Kind: ledger.KindArtifactRevisionAborted, Key: id, Payload: payload,
	}); err != nil {
		t.Fatalf("AppendIntent aborted: %v", err)
	}
}

// TestFold_SkipsAbortedRevision covers V4 §7 case 14's fold half: an aborted
// revision must not count as the id's latest, even though its
// artifact.revision entry landed first.
func TestFold_SkipsAbortedRevision(t *testing.T) {
	s := newMemStore(t)
	appendRevision(t, s, "chat1", "code_review:pr-1", 1, 0)
	appendRevision(t, s, "chat1", "code_review:pr-1", 2, 1)
	appendAborted(t, s, "chat1", "code_review:pr-1", 2)

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	a := res.Artifacts["code_review:pr-1"]
	latest, ok := a.Latest()
	if !ok || latest.Revision != 1 {
		t.Fatalf("latest = %+v, ok=%v; want revision 1", latest, ok)
	}
}

// TestFold_LaterEntryWins pre-#1144-P4 covered a retried save reusing its
// aborted attempt's revision number and parent; the store-level
// (chat_id, key, parent_revision) index now rejects that exact retry outright
// (AppendIntent returns ledger.ErrStaleParent) instead of ever landing a
// second entry to fold - see recordstore.saveAt's doc for the new,
// non-self-healing contract. Deleted with the rest of the aborted-retry path.

func TestLastRevision_NoEntries(t *testing.T) {
	s := newMemStore(t)
	rev, err := LastRevision(context.Background(), s, "chat1", "id1")
	if err != nil {
		t.Fatalf("LastRevision: %v", err)
	}
	if rev != 0 {
		t.Fatalf("LastRevision = %d, want 0", rev)
	}
}

// TestFold_NodeStatesLaterWins: a node's terminal status is its LAST
// node.done/failed entry; its StartedSeq is INDEPENDENTLY kept even after
// the node reaches a terminal state (#1121 - rebuild needs both).
func TestFold_NodeStatesLaterWins(t *testing.T) {
	s := newMemStore(t)
	mustAppend := func(kind string) int64 {
		payload, _ := json.Marshal(struct {
			NodeID string `json:"node_id"`
			Turn   string `json:"turn"`
			Round  int    `json:"round"`
		}{NodeID: "n1", Turn: "t1", Round: 2})
		seq, err := s.AppendIntent(context.Background(), ledger.Entry{
			ChatID: "chat1", Kind: kind, Payload: payload,
		})
		if err != nil {
			t.Fatalf("AppendIntent %s: %v", kind, err)
		}
		return seq
	}
	startedSeq := mustAppend(ledger.KindNodeStarted)
	doneSeq := mustAppend(ledger.KindNodeDone)

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	n := res.Nodes["n1"]
	if n == nil || n.TerminalStatus != "done" || n.TerminalSeq != doneSeq {
		t.Fatalf("node state = %+v, want terminal status done at seq %d", n, doneSeq)
	}
	if n.StartedSeq != startedSeq {
		t.Fatalf("node state = %+v, want StartedSeq %d preserved alongside the terminal state", n, startedSeq)
	}
}

// TestFold_NodeAcrossTurns_KeyedByNodeIDNotTurn is the #1125 review's
// blocking scenario: a turn is fresh per invocation, node IDs are only
// unique within one plan, so the SAME node ID legitimately recurs across
// turns. Turn 1's node N fails; turn 2's N (a later re-run) starts and
// completes. The fold must report exactly ONE current state for N - the
// live one - not two states (one per turn) that a consumer could resurrect
// turn 1's stale failure alongside turn 2's real success.
func TestFold_NodeAcrossTurns_KeyedByNodeIDNotTurn(t *testing.T) {
	s := newMemStore(t)
	append_ := func(turn, kind string) int64 {
		payload, _ := json.Marshal(struct {
			NodeID string `json:"node_id"`
			Turn   string `json:"turn"`
		}{NodeID: "n1", Turn: turn})
		seq, err := s.AppendIntent(context.Background(), ledger.Entry{
			ChatID: "chat1", Kind: kind, Payload: payload,
		})
		if err != nil {
			t.Fatalf("AppendIntent %s: %v", kind, err)
		}
		return seq
	}
	append_("turn-1", ledger.KindNodeStarted)
	append_("turn-1", ledger.KindNodeFailed) // turn 1: n1 fails
	turn2Start := append_("turn-2", ledger.KindNodeStarted)
	turn2Done := append_("turn-2", ledger.KindNodeDone) // turn 2: n1 (re-run) succeeds

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("Nodes has %d entries, want exactly 1 (one node, not one per turn): %+v", len(res.Nodes), res.Nodes)
	}
	n := res.Nodes["n1"]
	if n == nil {
		t.Fatalf("Nodes[\"n1\"] missing - node must be keyed by NodeID alone")
	}
	if n.TerminalStatus != "done" || n.TerminalSeq != turn2Done {
		t.Fatalf("node state = %+v, want turn 2's node_done (seq %d) - turn 1's stale node_failed must not survive", n, turn2Done)
	}
	if n.StartedSeq != turn2Start {
		t.Fatalf("node state = %+v, want StartedSeq %d (turn 2's start, not turn 1's)", n, turn2Start)
	}
	if n.TurnID != "turn-2" {
		t.Fatalf("node state = %+v, want TurnID turn-2 (the most recent)", n)
	}
}

// pagingFakeStore wraps MemStore and honors the requested limit exactly (like
// PGStore's real .Limit(n)), so shrinking fold's pageSize below the fixture
// count forces Fold through multiple pages, proving paged reads match one
// unpaged slice.
type pagingFakeStore struct {
	*ledger.MemStore
}

func (p *pagingFakeStore) ReadEntriesPage(ctx context.Context, chatID string, fromSeq int64, limit int) ([]ledger.Entry, error) {
	all, err := p.MemStore.ReadEntries(ctx, chatID, fromSeq)
	if err != nil {
		return nil, err
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func TestFold_PagingMatchesOneSlice(t *testing.T) {
	old := pageSize
	pageSize = 4
	defer func() { pageSize = old }()

	base := newMemStore(t)
	for i := 1; i <= 25; i++ {
		appendRevision(t, base, "chat1", "id1", i, i-1)
	}
	paged := &pagingFakeStore{MemStore: base}

	want, err := Fold(context.Background(), base, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold (unpaged): %v", err)
	}
	got, err := Fold(context.Background(), paged, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold (paged): %v", err)
	}
	wantLatest, _ := want.Artifacts["id1"].Latest()
	gotLatest, _ := got.Artifacts["id1"].Latest()
	if wantLatest.Revision != gotLatest.Revision || gotLatest.Revision != 25 {
		t.Fatalf("paged latest = %d, unpaged = %d, want 25", gotLatest.Revision, wantLatest.Revision)
	}
	if len(got.Artifacts["id1"].Revisions) != len(want.Artifacts["id1"].Revisions) {
		t.Fatalf("paged revisions = %d, unpaged = %d", len(got.Artifacts["id1"].Revisions), len(want.Artifacts["id1"].Revisions))
	}
}

// TestApply_FromCheckpointMatchesFromZero is #1144 P5's required proof: a
// fold seeded from a checkpoint must equal a fold from scratch. Three
// revisions land, a checkpoint is written after the second, then a third
// arrives after the checkpoint - Fold (checkpoint-aware) and Apply(..., -1)
// (always from scratch) must agree.
func TestApply_FromCheckpointMatchesFromZero(t *testing.T) {
	s := newMemStore(t)
	appendRevision(t, s, "chat1", "id1", 1, 0)
	appendRevision(t, s, "chat1", "id1", 2, 1)

	mid, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold before checkpoint: %v", err)
	}
	payload, err := EncodeCheckpoint(mid)
	if err != nil {
		t.Fatalf("EncodeCheckpoint: %v", err)
	}
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{
		ChatID: "chat1", Kind: ledger.KindCheckpoint, Payload: payload,
	}); err != nil {
		t.Fatalf("AppendIntent checkpoint: %v", err)
	}
	appendRevision(t, s, "chat1", "id1", 3, 2)

	fromCheckpoint, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold from checkpoint: %v", err)
	}
	fromScratch, err := applyFromScratchIgnoringCheckpoint(s, "chat1")
	if err != nil {
		t.Fatalf("fold from scratch: %v", err)
	}
	wantLatest, _ := fromScratch.Artifacts["id1"].Latest()
	gotLatest, _ := fromCheckpoint.Artifacts["id1"].Latest()
	if gotLatest.Revision != wantLatest.Revision || gotLatest.Revision != 3 {
		t.Fatalf("from checkpoint latest = %d, from scratch = %d, want 3", gotLatest.Revision, wantLatest.Revision)
	}
	if len(fromCheckpoint.Artifacts["id1"].Revisions) != len(fromScratch.Artifacts["id1"].Revisions) {
		t.Fatalf("from checkpoint revisions = %d, from scratch = %d",
			len(fromCheckpoint.Artifacts["id1"].Revisions), len(fromScratch.Artifacts["id1"].Revisions))
	}
	if fromCheckpoint.LastSeq != fromScratch.LastSeq {
		t.Fatalf("from checkpoint LastSeq = %d, from scratch = %d", fromCheckpoint.LastSeq, fromScratch.LastSeq)
	}
}

// applyFromScratchIgnoringCheckpoint folds every entry (including the
// checkpoint entry itself, which applyLoop's switch simply ignores) without
// ever consulting LastCheckpoint - the independent "ground truth" fold to
// compare a checkpoint-seeded fold against.
func applyFromScratchIgnoringCheckpoint(s ledger.LedgerStore, chatID string) (*Result, error) {
	entries, err := s.ReadEntries(context.Background(), chatID, 1)
	if err != nil {
		return nil, err
	}
	return applyEntries(entries), nil
}

// TestApply_StaleCheckpointStillFoldsCorrectly is the concurrent-turn case:
// a checkpoint appended AFTER newer entries already exist (its payload
// reflects an older LastSeq than the chat's real state - the checkpoint's
// own fold ran before those entries landed, but AppendIntent for it lost
// the race to append). Apply must trust the payload's LastSeq, not this
// entry's position in the log, or it would skip the entries that arrived
// between the fold and the append.
func TestApply_StaleCheckpointStillFoldsCorrectly(t *testing.T) {
	s := newMemStore(t)
	appendRevision(t, s, "chat1", "id1", 1, 0) // seq 1

	stale, err := Fold(context.Background(), s, "chat1", 0) // LastSeq=1
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	stalePayload, err := EncodeCheckpoint(stale)
	if err != nil {
		t.Fatalf("EncodeCheckpoint: %v", err)
	}

	appendRevision(t, s, "chat1", "id1", 2, 1) // seq 2, appended BEFORE the checkpoint
	// The checkpoint lands last (highest seq) but its payload still only
	// covers through seq 1 - simulating a concurrent turn racing ahead of it.
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{
		ChatID: "chat1", Kind: ledger.KindCheckpoint, Payload: stalePayload,
	}); err != nil {
		t.Fatalf("AppendIntent stale checkpoint: %v", err)
	}

	got, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	latest, ok := got.Artifacts["id1"].Latest()
	if !ok || latest.Revision != 2 {
		t.Fatalf("latest revision = %+v, ok=%v, want revision 2 (a stale checkpoint must not hide seq 2)", latest, ok)
	}
}
