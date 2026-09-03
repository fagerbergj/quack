package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/store"
)

const testKind = "ledgertest_doc"

func init() {
	recordstore.Register(testKind, recordstore.KindSpec{
		Class: recordstore.Structured,
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

	raw, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
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
// counts but leaves the drifted row untouched.
func TestRunLedgerRebuild_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"

	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)
	id, rev, err := c.SaveStructured(ctx, testKind, map[string]string{"hello": "world"}, "doc-1", recordstore.Lineage{})
	if err != nil {
		t.Fatalf("SaveStructured: %v", err)
	}
	if err := artifacts.UpdateArtifactMeta(ctx, appName, userID, chatID, id, int64(rev), "WRONG_KIND", "WRONG_CLASS", []byte(`{}`)); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, true)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if !report.DryRun || report.ArtifactRevisionsUpdated != 1 {
		t.Fatalf("report = %+v, want dry-run with 1 pending update", report)
	}

	_, lineage, _, _, err := c.LatestWithMeta(ctx, id)
	if err != nil {
		t.Fatalf("LatestWithMeta: %v", err)
	}
	if lineage.Author != "" {
		t.Fatalf("dry-run wrote lineage: %+v", lineage)
	}
}

// TestRunLedgerRebuild_RegeneratesSSETable is V4 §7 case 14's SSE side: node
// lifecycle entries in the WAL become node_start/node_done rows in the
// (cleared) SSE table.
func TestRunLedgerRebuild_RegeneratesSSETable(t *testing.T) {
	ctx := context.Background()
	st, ls, artifacts := newTestStack(t)
	const chatID = "chat-1"

	payload, err := json.Marshal(struct {
		NodeID string `json:"node_id"`
		Turn   string `json:"turn"`
		Round  int    `json:"round"`
	}{NodeID: "n1", Turn: "t1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeStarted, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent started: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: ledger.KindNodeDone, Payload: payload}); err != nil {
		t.Fatalf("AppendIntent done: %v", err)
	}

	report, err := RunLedgerRebuild(ctx, ls, st, artifacts, chatID, false)
	if err != nil {
		t.Fatalf("RunLedgerRebuild: %v", err)
	}
	if report.SSEEventsWritten != 1 {
		t.Fatalf("SSEEventsWritten = %d, want 1 (n1's final state)", report.SSEEventsWritten)
	}
	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("LoadChatEvents returned %d rows, want 1", len(evs))
	}
}

func TestRunLedgerShow_PrintsJSONLines(t *testing.T) {
	ctx := context.Background()
	_, ls, artifacts := newTestStack(t)
	const chatID, appName, userID = "chat-1", "quack", "local"
	c := recordstore.New(artifacts, appName, userID, chatID).WithLedger(ls)
	if _, _, err := c.SaveStructured(ctx, testKind, map[string]string{"a": "b"}, "doc-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("SaveStructured: %v", err)
	}

	var buf bytes.Buffer
	if err := RunLedgerShow(ctx, &buf, ls, chatID, 0); err != nil {
		t.Fatalf("RunLedgerShow: %v", err)
	}
	var e ledger.Entry
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &e); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if e.Kind != ledger.KindArtifactRevision {
		t.Fatalf("first entry kind = %q, want %q", e.Kind, ledger.KindArtifactRevision)
	}
}
