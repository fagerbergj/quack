package tools

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
)

// fakeToolEmbedder returns a fixed vector for every query - only the scope
// filter decides what recall_memory sees.
type fakeToolEmbedder struct{}

func (fakeToolEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// echoToolConsolidator writes its input verbatim as a single ADD op - enough
// to seed a bucket through the real Store.Commit path.
type echoToolConsolidator struct{ content string }

func (echoToolConsolidator) Name() string { return "echo-tool-consolidator" }

func (c echoToolConsolidator) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		reply := `{"ops":[{"action":"ADD","content":"` + c.content + `","kind":"note"}]}`
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// TestNewRecallMemory_LogsLedgerEntryWithCoords covers epic #1255 P2's native-
// worker verification: a call appends one memory.recall ledger entry with
// source "tool" and the coords ledger.StampCoords restamped onto the tool
// after Build (dag/graph.go), not the zero value.
func TestNewRecallMemory_LogsLedgerEntryWithCoords(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeToolEmbedder{}, echoToolConsolidator{content: "the build uses bazel"}, "test_recall_tool", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// Role: "task" - matches the tool's MemoryRole below (no Workspace
	// configured in this test, so Repo never resolves).
	if _, err := store.Commit(ctx, memory.Scope{Role: "task"}, "explorer", memory.Provenance{}, []memory.Candidate{{Content: "the build uses bazel"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}

	lgr := ledgertest.NewMemStore()
	tl, err := newRecallMemory(Deps{Memory: store, Ledger: lgr, MemoryRole: "task"})
	if err != nil {
		t.Fatalf("newRecallMemory: %v", err)
	}
	cs, ok := tl.(ledger.CoordSetter)
	if !ok {
		t.Fatal("recall_memory tool does not implement ledger.CoordSetter")
	}
	cs.SetLedgerCoords(ledger.Coords{ChatID: "chat1", Node: "node1"})

	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("recall_memory tool does not implement runnableTool")
	}
	out, err := rt.Run(newFakeCtx(), map[string]any{"query": "build system"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Run's result is a map[string]any (functiontool's own JSON-schema
	// conversion, not a direct Go type assertion) - roundtrip it back into
	// the typed shape, same as recallMemoryHits does for a real session event.
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal Run result: %v", err)
	}
	var result recallMemoryResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal Run result: %v", err)
	}
	if len(result.Hits) != 1 || !strings.Contains(result.Hits[0].Content, "the build uses bazel") {
		t.Fatalf("Run result = %+v, want one hit for the seeded memory", out)
	}

	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryRecall {
		t.Fatalf("entries = %+v, want exactly one memory.recall", entries)
	}
	if entries[0].NodeID != "node1" {
		t.Fatalf("NodeID = %q, want %q (the coords SetLedgerCoords restamped)", entries[0].NodeID, "node1")
	}
}

// TestRecallScope_NoNodeIDLegacyBucket: #1262/#1263 - recallScope's bucket
// list is exactly [role:..., ...], never a Legacy bucket keyed by the raw
// node id. Asserts the Scope directly (Commit never routes a Legacy-only
// scope to a real bucket, so a round-trip-through-Commit test here would be
// vacuous - it would pass even with Legacy: coords.Node reintroduced).
func TestRecallScope_NoNodeIDLegacyBucket(t *testing.T) {
	sc := recallScope(Deps{MemoryRole: "task"}, newFakeCtx(), ledger.Coords{ChatID: "chat1", Node: "node1"})
	want := []string{"role:task"}
	buckets := sc.Buckets()
	if len(buckets) != len(want) || buckets[0] != want[0] {
		t.Fatalf("Buckets() = %v, want %v (no node-id legacy bucket)", buckets, want)
	}
}

// TestNewRecallMemory_EmptyQueryRejected guards the trivial input-validation
// boundary every other quack tool enforces (stage_memory, commit_memory).
func TestNewRecallMemory_EmptyQueryRejected(t *testing.T) {
	store, err := memory.OpenSQLite(context.Background(), t.TempDir()+"/mem.db", fakeToolEmbedder{}, echoToolConsolidator{content: "x"}, "test_recall_empty", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	tl, err := newRecallMemory(Deps{Memory: store})
	if err != nil {
		t.Fatalf("newRecallMemory: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newFakeCtx(), map[string]any{"query": "  "}); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

// TestNewRecallMemory_NilStoreIsSafe matches stage_memory's own leniency: a
// Deps with no Memory store still builds (a tools.Build caller may resolve
// tool names ahead of the real per-agent wiring - see
// TestNoNativeAgentGrantedGitHubWriteTool), and simply recalls nothing.
func TestNewRecallMemory_NilStoreIsSafe(t *testing.T) {
	tl, err := newRecallMemory(Deps{})
	if err != nil {
		t.Fatalf("newRecallMemory with no Memory store must still build, got: %v", err)
	}
	rt := tl.(runnableTool)
	out, err := rt.Run(newFakeCtx(), map[string]any{"query": "anything"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out["hits"] != nil {
		t.Fatalf("Run result = %+v, want no hits with no store configured", out)
	}
}
