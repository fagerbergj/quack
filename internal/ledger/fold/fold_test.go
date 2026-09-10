package fold

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

func newMemStore(t *testing.T) *ledgertest.MemStore {
	t.Helper()
	return ledgertest.NewMemStore()
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

// TestFold_LaterEntryWins (pre-#1144-P4: retried save reusing its aborted attempt's
// revision number) is deleted with the aborted-retry path - the (chat_id, key,
// parent_revision) index now rejects that retry outright with ErrStaleParent (see recordstore.saveAt's doc).

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

// TestFold_NodeAcrossTurns_KeyedByNodeIDNotTurn (#1125 review): node IDs are only unique
// within one plan, so the same ID recurs across turns (turn 1's N fails, turn 2's N
// completes) - the fold must report exactly ONE current state, not two states resurrecting the stale failure.
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
// PGStore's real .Limit(n)), so shrinking pageSize below the fixture count forces
// Fold through multiple pages, proving paged reads match one unpaged slice.
type pagingFakeStore struct {
	*ledgertest.MemStore
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

// TestApplySeeded_FromCheckpointMatchesFromZero (#1144 P5): a fold seeded from a
// checkpoint must equal a fold from scratch - two revisions are checkpointed, a
// third arrives, and ApplySeeded and Apply must agree (checkpoint = plain *Result, as store.Checkpoint's row holds).
func TestApplySeeded_FromCheckpointMatchesFromZero(t *testing.T) {
	s := newMemStore(t)
	appendRevision(t, s, "chat1", "id1", 1, 0)
	appendRevision(t, s, "chat1", "id1", 2, 1)

	checkpoint, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold before checkpoint: %v", err)
	}
	appendRevision(t, s, "chat1", "id1", 3, 2)

	fromCheckpoint, err := ApplySeeded(context.Background(), s, "chat1", checkpoint, 0)
	if err != nil {
		t.Fatalf("ApplySeeded: %v", err)
	}
	fromScratch, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold from scratch: %v", err)
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

// TestApplySeeded_StaleCheckpointStillFoldsCorrectly: a checkpoint whose own LastSeq is
// older than entries already in the ledger (WriteCheckpoint's read-fold-write window)
// still folds correctly - ApplySeeded trusts seed.LastSeq, not the caller's `from`.
func TestApplySeeded_StaleCheckpointStillFoldsCorrectly(t *testing.T) {
	s := newMemStore(t)
	appendRevision(t, s, "chat1", "id1", 1, 0) // seq 1
	appendMemoryRecall(t, s, "chat1", "m1")    // recalls[m1] = 1, folded into the checkpoint below
	appendMemoryVote(t, s, "chat1", "m1", ledger.MemoryVoteSupported)

	stale, err := Fold(context.Background(), s, "chat1", 0) // LastSeq=3
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if stale.MemoryRecalls["m1"].Recalls != 1 || stale.MemoryVotes["m1"].Upvotes != 1 {
		t.Fatalf("checkpoint memory state = %+v / %+v, want recalls=1 upvotes=1", stale.MemoryRecalls["m1"], stale.MemoryVotes["m1"])
	}

	appendRevision(t, s, "chat1", "id1", 2, 1)                           // seq 4, lands after the checkpoint fold ran
	appendMemoryRecall(t, s, "chat1", "m1")                              // seq 5: a second recall of the same memory
	appendMemoryVote(t, s, "chat1", "m1", ledger.MemoryVoteContradicted) // seq 6: a later downvote

	// A caller passing from=0 (as if it didn't know any better) must still
	// get the right answer, because ApplySeeded reads from seed.LastSeq.
	got, err := ApplySeeded(context.Background(), s, "chat1", stale, 0)
	if err != nil {
		t.Fatalf("ApplySeeded: %v", err)
	}
	latest, ok := got.Artifacts["id1"].Latest()
	if !ok || latest.Revision != 2 {
		t.Fatalf("latest revision = %+v, ok=%v, want revision 2 (a stale checkpoint must not hide seq 2)", latest, ok)
	}

	// seedMemory's deep copy: the checkpoint's own MemoryRecalls/MemoryVotes
	// maps must be untouched by ApplySeeded accumulating on top of a copy.
	if stale.MemoryRecalls["m1"].Recalls != 1 || stale.MemoryVotes["m1"].Upvotes != 1 || stale.MemoryVotes["m1"].Downvotes != 0 {
		t.Fatalf("seed mutated in place: recalls=%+v votes=%+v, want unchanged (1 recall, 1 upvote, 0 downvotes)",
			stale.MemoryRecalls["m1"], stale.MemoryVotes["m1"])
	}
	// The returned Result accumulates the checkpoint's seeded state PLUS
	// what landed after it - not just what's in the post-checkpoint entries.
	if got.MemoryRecalls["m1"].Recalls != 2 {
		t.Fatalf("m1 recalls = %d, want 2 (1 seeded + 1 after the checkpoint)", got.MemoryRecalls["m1"].Recalls)
	}
	if v := got.MemoryVotes["m1"]; v.Upvotes != 1 || v.Downvotes != 1 {
		t.Fatalf("m1 votes = %+v, want upvotes=1 (seeded) downvotes=1 (after the checkpoint)", v)
	}
	if got.MemoryRecalls["m1"].LastRecalledAt.IsZero() {
		t.Fatal("m1 LastRecalledAt = zero, want the second recall's timestamp")
	}
	if got.MemoryVotes["m1"].LastUpvotedAt.IsZero() {
		t.Fatal("m1 LastUpvotedAt = zero, want the seeded (first, supported) vote's timestamp - the later vote was a downvote and must not clear it")
	}
}

func appendMemoryRecall(t *testing.T, s ledger.LedgerStore, chatID string, ids ...string) {
	t.Helper()
	entries := make([]ledger.MemoryRecallEntry, len(ids))
	for i, id := range ids {
		entries[i] = ledger.MemoryRecallEntry{ID: id}
	}
	payload, err := json.Marshal(ledger.MemoryRecallPayload{Source: "prefill", Entries: entries})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{ChatID: chatID, Kind: ledger.KindMemoryRecall, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent memory.recall: %v", err)
	}
}

func appendMemoryVote(t *testing.T, s ledger.LedgerStore, chatID, memoryID string, vote ledger.MemoryVote) {
	t.Helper()
	appendMemoryVoteAs(t, s, chatID, memoryID, vote, "judge")
}

func appendMemoryVoteAs(t *testing.T, s ledger.LedgerStore, chatID, memoryID string, vote ledger.MemoryVote, actor string) {
	t.Helper()
	payload, err := json.Marshal(ledger.MemoryVotePayload{MemoryID: memoryID, Vote: vote, Actor: actor})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.AppendIntent(context.Background(), ledger.Entry{ChatID: chatID, Kind: ledger.KindMemoryVote, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent memory.vote: %v", err)
	}
}

// TestFold_MemoryRecallAndVoteProjections (epic #1255 P1 rebuild requirement):
// recall/vote entries fold into per-id counts and tallies, and RecalledIDs names
// exactly the recalled set, not minted-but-never-recalled ids.
func TestFold_MemoryRecallAndVoteProjections(t *testing.T) {
	s := newMemStore(t)
	appendMemoryRecall(t, s, "chat1", "m1", "m2")
	appendMemoryRecall(t, s, "chat1", "m1") // m1 recalled again in a later round
	appendMemoryVote(t, s, "chat1", "m1", ledger.MemoryVoteSupported)
	appendMemoryVote(t, s, "chat1", "m2", ledger.MemoryVoteContradicted)

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	if res.MemoryRecalls["m1"].Recalls != 2 {
		t.Fatalf("m1 recalls = %d, want 2", res.MemoryRecalls["m1"].Recalls)
	}
	if res.MemoryRecalls["m2"].Recalls != 1 {
		t.Fatalf("m2 recalls = %d, want 1", res.MemoryRecalls["m2"].Recalls)
	}
	if res.MemoryVotes["m1"].Upvotes != 1 || res.MemoryVotes["m1"].Downvotes != 0 {
		t.Fatalf("m1 votes = %+v, want 1 upvote 0 downvotes", res.MemoryVotes["m1"])
	}
	if res.MemoryVotes["m2"].Downvotes != 1 || res.MemoryVotes["m2"].Upvotes != 0 {
		t.Fatalf("m2 votes = %+v, want 0 upvotes 1 downvote", res.MemoryVotes["m2"])
	}

	ids := res.RecalledIDs()
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Fatalf("RecalledIDs = %v, want [m1 m2]", ids)
	}

	// A second fold from scratch (a rebuild) reproduces the same projections.
	rebuilt, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold (rebuild): %v", err)
	}
	if rebuilt.MemoryRecalls["m1"].Recalls != 2 || rebuilt.MemoryVotes["m1"].Upvotes != 1 {
		t.Fatalf("rebuild projections = %+v / %+v, want unchanged from the first fold", rebuilt.MemoryRecalls["m1"], rebuilt.MemoryVotes["m1"])
	}
}

// TestFold_HumanVoteTogglesInsteadOfStacking (#1265 review finding 4): a human's
// repeated votes are a toggle, not judge-style additive stacking - up -> none ->
// down leaves exactly one downvote; a judge vote in between stays purely additive.
func TestFold_HumanVoteTogglesInsteadOfStacking(t *testing.T) {
	s := newMemStore(t)
	appendMemoryVoteAs(t, s, "chat1", "m1", ledger.MemoryVoteSupported, "human")    // up
	appendMemoryVoteAs(t, s, "chat1", "m1", ledger.MemoryVoteNotRelevant, "human")  // none (retract)
	appendMemoryVoteAs(t, s, "chat1", "m1", ledger.MemoryVoteContradicted, "human") // down
	appendMemoryVote(t, s, "chat1", "m2", ledger.MemoryVoteSupported)               // unrelated judge vote

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	m1 := res.MemoryVotes["m1"]
	if m1.Upvotes != 0 || m1.Downvotes != 1 {
		t.Fatalf("m1 votes after up->none->down = %+v, want 0 upvotes 1 downvote", m1)
	}
	if m2 := res.MemoryVotes["m2"]; m2.Upvotes != 1 {
		t.Fatalf("m2 (judge, additive, untouched by m1's toggling) = %+v, want 1 upvote", m2)
	}
}

// TestFoldAbsorption_RedirectsVotesAndRecallsToSurvivor (epic #1255 P5): votes/recalls
// cast before a consolidation-merge are still attributed to the survivor via
// absorbedBy (AbsorbedIDs), covering a chain - a rebuild must not lose history to an id the store no longer serves.
func TestFoldAbsorption_RedirectsVotesAndRecallsToSurvivor(t *testing.T) {
	s := newMemStore(t)
	appendMemoryRecall(t, s, "chat1", "dup", "dup2", "survivor")
	appendMemoryVote(t, s, "chat1", "dup", ledger.MemoryVoteSupported)
	appendMemoryVote(t, s, "chat1", "dup2", ledger.MemoryVoteSupported)
	appendMemoryVote(t, s, "chat1", "survivor", ledger.MemoryVoteContradicted)

	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}

	res.FoldAbsorption(map[string]string{"dup": "survivor", "dup2": "survivor"})

	if _, ok := res.MemoryVotes["dup"]; ok {
		t.Fatal("dup's votes should have been folded away, not left standing")
	}
	sv := res.MemoryVotes["survivor"]
	if sv == nil || sv.Upvotes != 2 || sv.Downvotes != 1 {
		t.Fatalf("survivor votes after fold = %+v, want 2 upvotes (from dup+dup2) + 1 downvote (its own)", sv)
	}
	if res.MemoryRecalls["survivor"].Recalls != 3 {
		t.Fatalf("survivor recalls after fold = %d, want 3 (its own + dup's + dup2's)", res.MemoryRecalls["survivor"].Recalls)
	}
}

// TestFoldAbsorption_EmptyMapIsNoop guards the common case (no absorption
// has ever happened) against needlessly rebuilding the maps.
func TestFoldAbsorption_EmptyMapIsNoop(t *testing.T) {
	s := newMemStore(t)
	appendMemoryVote(t, s, "chat1", "m1", ledger.MemoryVoteSupported)
	res, err := Fold(context.Background(), s, "chat1", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	before := res.MemoryVotes["m1"]
	res.FoldAbsorption(nil)
	if res.MemoryVotes["m1"] != before {
		t.Fatal("FoldAbsorption(nil) must leave MemoryVotes untouched")
	}
}
