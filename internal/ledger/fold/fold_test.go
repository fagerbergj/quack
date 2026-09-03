package fold

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

func newFSStore(t *testing.T) *ledger.FSStore {
	t.Helper()
	s, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	return s
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
	s := newFSStore(t)
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

// TestFold_LaterEntryWins: a retried save reuses the same revision number
// its aborted attempt claimed - the later artifact.revision entry must
// re-materialize it, per KindArtifactRevisionAborted's doc.
func TestFold_LaterEntryWins(t *testing.T) {
	s := newFSStore(t)
	appendRevision(t, s, "chat1", "id1", 1, 0)
	appendAborted(t, s, "chat1", "id1", 1)
	appendRevision(t, s, "chat1", "id1", 1, 0) // retry, same number, now succeeds

	rev, err := LastRevision(context.Background(), s, "chat1", "id1")
	if err != nil {
		t.Fatalf("LastRevision: %v", err)
	}
	if rev != 1 {
		t.Fatalf("LastRevision = %d, want 1", rev)
	}
}

func TestLastRevision_NoEntries(t *testing.T) {
	s := newFSStore(t)
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
	s := newFSStore(t)
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
	s := newFSStore(t)
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

// pagingFakeStore wraps FSStore and honors the requested limit exactly (like
// PGStore's real .Limit(n)), so shrinking fold's pageSize below the fixture
// count forces Fold through multiple pages, proving paged reads match one
// unpaged slice.
type pagingFakeStore struct {
	*ledger.FSStore
}

func (p *pagingFakeStore) ReadEntriesPage(ctx context.Context, chatID string, fromSeq int64, limit int) ([]ledger.Entry, error) {
	all, err := p.FSStore.ReadEntries(ctx, chatID, fromSeq)
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

	base := newFSStore(t)
	for i := 1; i <= 25; i++ {
		appendRevision(t, base, "chat1", "id1", i, i-1)
	}
	paged := &pagingFakeStore{FSStore: base}

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
