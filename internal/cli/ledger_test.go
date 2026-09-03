package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
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

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if report.ArtifactRevisionsUpdated != 1 {
		t.Fatalf("ArtifactRevisionsUpdated = %d, want 1", report.ArtifactRevisionsUpdated)
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

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, true)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if !report.DryRun || report.ArtifactRevisionsUpdated != 1 {
		t.Fatalf("report = %+v, want dry-run with 1 pending update", report)
	}

	_, _, lineage, _, _, err := c.LatestWithMeta(ctx, id)
	if err != nil {
		t.Fatalf("LatestWithMeta: %v", err)
	}
	if lineage.Author != "DRIFTED" || lineage.NodeID != "WRONG_NODE" || lineage.Round != 99 {
		t.Fatalf("dry-run wrote lineage: got %+v, want the drifted value untouched", lineage)
	}
}

// TestRunLedgerRebuild_RegeneratesSSETable is V4 §7 case 14's SSE side - and
// its honest ceiling: rebuild reconstructs node LIFECYCLE only (which node,
// which terminal status), never the richer live payload (tokens, output,
// model) the skinny node.* WAL entry never carried. It asserts the event's
// actual content (id, node id, name), not just a row count, so a rebuild
// that silently wrote the wrong node or the wrong terminal state would fail.
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
	doneSeq, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload("n1")})
	if err != nil {
		t.Fatalf("AppendIntent n1 done: %v", err)
	}
	// A second node, still running (no done/failed) - its LAST entry is
	// node.started, so it must come back as node_start, not node_done.
	startedSeq, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload("n2")})
	if err != nil {
		t.Fatalf("AppendIntent n2 started: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if report.SSEEventsWritten != 2 {
		t.Fatalf("SSEEventsWritten = %d, want 2 (n1's and n2's final states)", report.SSEEventsWritten)
	}

	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("LoadChatEvents returned %d rows, want 2", len(evs))
	}
	byNode := map[string]struct {
		seq  int64
		name string
	}{}
	for _, row := range evs {
		ev, err := runlog.UnmarshalEvent(row.Event)
		if err != nil {
			t.Fatalf("UnmarshalEvent: %v", err)
		}
		var d struct {
			NodeID string `json:"node_id"`
		}
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			t.Fatalf("marshal event data: %v", err)
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("unmarshal event data: %v", err)
		}
		byNode[d.NodeID] = struct {
			seq  int64
			name string
		}{seq: row.Seq, name: ev.Name}
	}

	n1, ok := byNode["n1"]
	if !ok || n1.name != "node_done" || n1.seq != doneSeq {
		t.Fatalf("n1 event = %+v (ok=%v), want node_done at seq %d", n1, ok, doneSeq)
	}
	n2, ok := byNode["n2"]
	if !ok || n2.name != "node_start" || n2.seq != startedSeq {
		t.Fatalf("n2 event = %+v (ok=%v), want node_start at seq %d", n2, ok, startedSeq)
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
