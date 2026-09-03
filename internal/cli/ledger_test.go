package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

const testKind = "ledgertest_doc"

func init() {
	recordstore.Register(testKind, recordstore.KindSpec{
		Class:      recordstore.Structured,
		JSONSchema: `{"type":"object"}`,
		Identity: func(content []byte, hint string) (string, error) {
			return hint, nil
		},
	})
}

func newTestStack(t *testing.T) (*store.Store, ledger.LedgerStore, *store.TurnAwareService) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ls, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	rowSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	artifacts := store.NewTurnAwareService(rowSvc)
	return st, ls, artifacts
}

// TestRunLedgerRebuild_RegeneratesArtifactMeta is V4 §7 case 14's artifact
// side: after wiping a revision's kind/class/lineage columns, rebuild must
// restore them from the ledger fold - bytes are untouched throughout.
func TestRunLedgerRebuild_RegeneratesArtifactMeta(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"

	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)
	id, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"hello": "world"}, "doc-1", recordstore.Lineage{Author: "tester"})
	if err != nil {
		t.Fatalf("SaveStructured: %v", err)
	}

	// Drift the row's metadata to prove rebuild actually overwrites it.
	if err := artifacts.UpdateArtifactMeta(ctx, appName, userID, chatID, id, int64(rev), "WRONG_KIND", "WRONG_CLASS", []byte(`{}`)); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if report.ArtifactRevisionsChanged != 1 {
		t.Fatalf("ArtifactRevisionsChanged = %d, want 1", report.ArtifactRevisionsChanged)
	}
	if len(report.ArtifactUpdateErrors) != 0 {
		t.Fatalf("ArtifactUpdateErrors = %v, want none", report.ArtifactUpdateErrors)
	}

	raw, _, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
	if err != nil || !ok {
		t.Fatalf("LatestWithMeta: ok=%v err=%v", ok, err)
	}
	if gotRev != rev {
		t.Fatalf("revision drifted: got %d, want %d (rebuild must not touch bytes/revision)", gotRev, rev)
	}
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil || doc["hello"] != "world" {
		t.Fatalf("bytes changed by rebuild: %s", raw)
	}
	if lineage.Author != "tester" {
		t.Fatalf("lineage not restored: %+v", lineage)
	}
}

// TestRunLedgerRebuild_DryRunWritesNothing: --dry-run reports the same
// counts but leaves the drifted row untouched. Seeds a REAL lineage
// (Author/NodeID/Round all set) and drifts it to a DIFFERENT real lineage,
// so this test cannot pass vacuously (an empty-vs-empty lineage comparison
// would pass even if dry-run silently wrote - #1111 review finding).
func TestRunLedgerRebuild_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"

	original := recordstore.Lineage{Author: "tester", NodeID: "n1", Round: 2}
	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)
	id, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"hello": "world"}, "doc-1", original)
	if err != nil {
		t.Fatalf("SaveStructured: %v", err)
	}
	drift, err := json.Marshal(recordstore.Lineage{Author: "DRIFTED", NodeID: "WRONG_NODE", Round: 99})
	if err != nil {
		t.Fatalf("marshal drift: %v", err)
	}
	if err := artifacts.UpdateArtifactMeta(ctx, appName, userID, chatID, id, int64(rev), "WRONG_KIND", "WRONG_CLASS", drift); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, true, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if !report.DryRun || report.ArtifactRevisionsChanged != 1 {
		t.Fatalf("report = %+v, want dry-run with 1 pending change", report)
	}

	_, _, lineage, _, _, err := c.LatestWithMeta(ctx, id)
	if err != nil {
		t.Fatalf("LatestWithMeta: %v", err)
	}
	if lineage.Author != "DRIFTED" || lineage.NodeID != "WRONG_NODE" || lineage.Round != 99 {
		t.Fatalf("dry-run wrote lineage: got %+v, want the drifted value untouched", lineage)
	}
}

// TestRunLedgerRebuild_RegeneratesSSETable is #1121's "missing lifecycle
// rows are inserted" case (the table starts EMPTY - every row is missing) -
// and rebuild's honest ceiling: it reconstructs node LIFECYCLE only (which
// node, which terminal status), never the richer live payload (tokens,
// output, model) the skinny node.* WAL entry never carried. It asserts the
// event's actual content (id, node id, name), not just a row count, so a
// rebuild that silently wrote the wrong node or the wrong terminal state
// would fail.
func TestRunLedgerRebuild_RegeneratesSSETable(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID = "chat-1"

	payload := func(nodeID string) []byte {
		b, err := json.Marshal(struct {
			NodeID string `json:"node_id"`
			Turn   string `json:"turn"`
			Round  int    `json:"round"`
		}{NodeID: nodeID, Turn: "t1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload("n1")}); err != nil {
		t.Fatalf("AppendIntent n1 started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload("n1")}); err != nil {
		t.Fatalf("AppendIntent n1 done: %v", err)
	}
	// A second node, still running (no done/failed) - its LAST entry is
	// node.started, so it must come back as node_start, not node_done.
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload("n2")}); err != nil {
		t.Fatalf("AppendIntent n2 started: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	// n1 gets BOTH node_start and node_done (#1121: tracked independently),
	// n2 gets node_start only - 3 rows, all missing since the table started empty.
	if report.SSERowsInserted != 3 {
		t.Fatalf("SSERowsInserted = %d, want 3 (n1 start+done, n2 start)", report.SSERowsInserted)
	}

	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("LoadChatEvents returned %d rows, want 3", len(evs))
	}
	type seen struct {
		seq  int64
		name string
	}
	byNode := map[string][]seen{}
	for _, row := range evs {
		ev, err := runlog.UnmarshalEvent(row.Event)
		if err != nil {
			t.Fatalf("UnmarshalEvent: %v", err)
		}
		nodeID, ok := runlog.EventNodeID(ev)
		if !ok {
			t.Fatalf("event %+v has no extractable node id", ev)
		}
		byNode[nodeID] = append(byNode[nodeID], seen{seq: row.Seq, name: ev.Name})
	}

	n1 := byNode["n1"]
	if len(n1) != 2 || n1[0].name != "node_start" || n1[1].name != "node_done" || n1[0].seq >= n1[1].seq {
		t.Fatalf("n1 events = %+v, want [node_start, node_done] in that seq order", n1)
	}
	n2 := byNode["n2"]
	if len(n2) != 1 || n2[0].name != "node_start" {
		t.Fatalf("n2 events = %+v, want exactly [node_start]", n2)
	}
}

// TestRunLedgerRebuild_HealthyChatIsANoop is #1121's core regression: on a
// chat whose artifact metadata and lifecycle rows ALREADY match the ledger,
// --dry-run reports zero changes and a REAL rebuild changes nothing at all -
// same row count, same row content, same artifact metadata, before and
// after. This is the exact scenario that used to replace ~4000 chat_events
// rows with 3 synthesized ones.
func TestRunLedgerRebuild_HealthyChatIsANoop(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"

	// A real artifact revision, saved normally - its stored kind/class/
	// lineage is EXACTLY what recordstore wrote, so the fold agrees with it.
	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)
	if _, _, err := c.SaveStructured(ctx, testKind, map[string]string{"hello": "world"}, "doc-1", recordstore.Lineage{Author: "tester"}); err != nil {
		t.Fatalf("SaveStructured: %v", err)
	}

	// A node that already reached done, PLUS its own node_start and
	// node_done rows already in the table (a healthy run leaves both) and
	// a chunk of observational history the ledger has no source for at all.
	payload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
		Round  int    `json:"round"`
	}{NodeID: "n1", Turn: "t1"})
	if err != nil {
		t.Fatalf("marshal node payload: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent n1 started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent n1 done: %v", err)
	}
	seedEvent(t, ctx, st, chatID, 1, stream.NodeStart("n1", "worker"))
	seedEvent(t, ctx, st, chatID, 2, stream.SSEEvent{Name: stream.EventAgentToken, Data: stream.AgentTokenData{RunID: "r1", Text: "hello"}})
	seedEvent(t, ctx, st, chatID, 3, stream.SSEEvent{Name: stream.EventAgentToken, Data: stream.AgentTokenData{RunID: "r1", Text: " world"}})
	seedEvent(t, ctx, st, chatID, 4, stream.NodeDone("n1", stream.NodeDoneData{Output: "hello world", Model: "gpt-real"}))

	before, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents before: %v", err)
	}
	if len(before) != 4 {
		t.Fatalf("seeded %d rows, want 4", len(before))
	}

	dry, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, true, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild dry-run: %v", err)
	}
	if dry.ArtifactRevisionsChanged != 0 || dry.SSERowsInserted != 0 {
		t.Fatalf("dry-run on a healthy chat reported changes: %+v, want zero", dry)
	}

	real, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if real.ArtifactRevisionsChanged != 0 || real.SSERowsInserted != 0 {
		t.Fatalf("rebuild of a healthy chat reported changes: %+v, want zero", real)
	}

	after, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("row count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("row %d changed:\nbefore=%+v\nafter=%+v", i, before[i], after[i])
		}
	}
}

// TestRunLedgerRebuild_NodeAcrossTurnsIsStillANoop is the #1125 review's
// blocking scenario end-to-end: turn 1's node N fails, turn 2's N (a later
// re-run, fresh turn id) completes - the CURRENT table (per-run) only ever
// holds turn 2's rows. Rebuild must not resurrect turn 1's stale
// node_failed (nor insert a second node_start) - it must be a no-op,
// exactly like a chat with only one turn per node.
func TestRunLedgerRebuild_NodeAcrossTurnsIsStillANoop(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID = "chat-1"

	payload := func(turn string) []byte {
		b, err := json.Marshal(struct {
			NodeID string `json:"node_id"`
			Turn   string `json:"turn"`
		}{NodeID: "n1", Turn: turn})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	// Turn 1: n1 fails. Its table rows were cleared by EventLog.Reset when
	// turn 2 started (a live run's real behavior) - nothing to seed for it.
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload("turn-1")}); err != nil {
		t.Fatalf("AppendIntent turn-1 started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeFailed, Payload: payload("turn-1")}); err != nil {
		t.Fatalf("AppendIntent turn-1 failed: %v", err)
	}
	// Turn 2: n1 (re-run) succeeds - the table holds exactly this.
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload("turn-2")}); err != nil {
		t.Fatalf("AppendIntent turn-2 started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload("turn-2")}); err != nil {
		t.Fatalf("AppendIntent turn-2 done: %v", err)
	}
	seedEvent(t, ctx, st, chatID, 1, stream.NodeStart("n1", "worker"))
	seedEvent(t, ctx, st, chatID, 2, stream.NodeDone("n1", stream.NodeDoneData{Output: "turn 2 succeeded"}))

	before, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("seeded %d rows, want 2", len(before))
	}

	dry, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, true, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild dry-run: %v", err)
	}
	if dry.SSERowsInserted != 0 {
		t.Fatalf("dry-run reported %d pending inserts, want 0 (turn 1's stale node_failed must not count)", dry.SSERowsInserted)
	}

	real, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if real.SSERowsInserted != 0 {
		t.Fatalf("rebuild inserted %d rows, want 0", real.SSERowsInserted)
	}

	after, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents after: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("row count = %d after rebuild, want 2 (unchanged) - turn 1's stale failure must not have been inserted", len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("row %d changed:\nbefore=%+v\nafter=%+v", i, before[i], after[i])
		}
	}
}

// TestRunLedgerRebuild_InsertsMissingWithoutTouchingOthers seeds a table
// with real observational rows and ONE existing lifecycle row, then folds a
// WAL with a genuinely missing second node's lifecycle - rebuild must add
// only that missing row and leave every other row (observational AND the
// other node's existing lifecycle row) byte-for-byte untouched.
func TestRunLedgerRebuild_InsertsMissingWithoutTouchingOthers(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID = "chat-1"

	nodePayload := func(nodeID string) []byte {
		b, err := json.Marshal(struct {
			NodeID string `json:"node_id"`
			Turn   string `json:"turn"`
		}{NodeID: nodeID, Turn: "t1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	// n1: already has BOTH ledger entries AND both table rows - untouched.
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: nodePayload("n1")}); err != nil {
		t.Fatalf("AppendIntent n1 started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: nodePayload("n1")}); err != nil {
		t.Fatalf("AppendIntent n1 done: %v", err)
	}
	// n2: has a ledger entry but NO table row at all - the missing case.
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: nodePayload("n2")}); err != nil {
		t.Fatalf("AppendIntent n2 started: %v", err)
	}

	seedEvent(t, ctx, st, chatID, 1, stream.NodeStart("n1", "worker"))
	seedEvent(t, ctx, st, chatID, 2, stream.SSEEvent{Name: stream.EventAgentToken, Data: stream.AgentTokenData{RunID: "r1", Text: "hi"}})
	seedEvent(t, ctx, st, chatID, 3, stream.NodeDone("n1", stream.NodeDoneData{Output: "real output, richer than any fold reconstruction"}))

	before, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents before: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if report.SSERowsInserted != 1 {
		t.Fatalf("SSERowsInserted = %d, want 1 (only n2's missing node_start)", report.SSERowsInserted)
	}

	after, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("row count = %d, want %d (before + exactly 1 insert)", len(after), len(before)+1)
	}
	// Every row from `before` must still be present, byte-for-byte, among `after`.
	for i, b := range before {
		if after[i] != b {
			t.Fatalf("existing row %d was touched:\nbefore=%+v\nafter=%+v", i, b, after[i])
		}
	}
	newRow := after[len(after)-1]
	ev, err := runlog.UnmarshalEvent(newRow.Event)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	nodeID, ok := runlog.EventNodeID(ev)
	if !ok || nodeID != "n2" || ev.Name != "node_start" {
		t.Fatalf("inserted row = %+v (node=%s), want n2's node_start", ev, nodeID)
	}
}

// seedEvent inserts one real ChatEvent row directly - a stand-in for what a
// live Publisher would have written, so tests can set up a table state
// without driving an actual run.
func seedEvent(t *testing.T, ctx context.Context, st *store.Store, chatID string, seq int64, ev stream.SSEEvent) {
	t.Helper()
	js, err := runlog.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: seq, Event: js, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("InsertChatEvent: %v", err)
	}
}

// TestRunLedgerShow_PrintsJSONLines exercises the JSONL contract `show`
// advertises across MULTIPLE entries and kinds - each line independently
// parseable, in seq order, with --from-seq's ">=" boundary honored - not
// just the one-entry case (#1111 review finding).
func TestRunLedgerShow_PrintsJSONLines(t *testing.T) {
	ctx := context.Background()
	_, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"
	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)

	// Two artifact revisions plus a node pair - three entries of two
	// different kinds, so ordering/kind assertions actually distinguish them.
	if _, _, err := c.SaveStructured(ctx, testKind, map[string]string{"a": "1"}, "doc-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("SaveStructured 1: %v", err)
	}
	if _, _, err := c.SaveStructured(ctx, testKind, map[string]string{"a": "2"}, "doc-1", recordstore.Lineage{ParentRevision: 1}); err != nil {
		t.Fatalf("SaveStructured 2: %v", err)
	}
	nodePayload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
	}{NodeID: "n1"})
	if err != nil {
		t.Fatalf("marshal node payload: %v", err)
	}
	lastSeq, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: nodePayload})
	if err != nil {
		t.Fatalf("AppendIntent node.started: %v", err)
	}

	// Unfiltered: all three entries, in seq order.
	var all bytes.Buffer
	if err := RunLedgerShow(ctx, &all, ls, chatID, 0); err != nil {
		t.Fatalf("RunLedgerShow: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(all.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3\n%s", len(lines), all.String())
	}
	var entries []ledger.Entry
	for i, line := range lines {
		var e ledger.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", i, err, line)
		}
		entries = append(entries, e)
	}
	wantKinds := []string{ledger.KindArtifactRevision, ledger.KindArtifactRevision, ledger.KindNodeStarted}
	for i, e := range entries {
		if e.Kind != wantKinds[i] {
			t.Fatalf("line %d kind = %q, want %q", i, e.Kind, wantKinds[i])
		}
		if i > 0 && entries[i-1].Seq >= e.Seq {
			t.Fatalf("entries not in seq order: line %d seq %d >= line %d seq %d", i-1, entries[i-1].Seq, i, e.Seq)
		}
	}

	// --from-seq boundary: exactly the node entry (its own seq is >= itself).
	var filtered bytes.Buffer
	if err := RunLedgerShow(ctx, &filtered, ls, chatID, lastSeq); err != nil {
		t.Fatalf("RunLedgerShow with fromSeq: %v", err)
	}
	filteredLines := bytes.Split(bytes.TrimSpace(filtered.Bytes()), []byte("\n"))
	if len(filteredLines) != 1 {
		t.Fatalf("--from-seq=%d returned %d lines, want 1\n%s", lastSeq, len(filteredLines), filtered.String())
	}
	var last ledger.Entry
	if err := json.Unmarshal(filteredLines[0], &last); err != nil {
		t.Fatalf("filtered line not valid JSON: %v", err)
	}
	if last.Seq != lastSeq || last.Kind != ledger.KindNodeStarted {
		t.Fatalf("filtered entry = %+v, want the node.started entry at seq %d", last, lastSeq)
	}
}
