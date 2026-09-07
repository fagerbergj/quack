package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestSeedProjectionWatermarks_SeparateStores proves seeding works across the
// real deployment topology (session store and ledger store as distinct
// instances, e.g. sqlite session store + a separate ledger DB) and is a
// no-op on a second boot.
func TestSeedProjectionWatermarks_SeparateStores(t *testing.T) {
	ctx := context.Background()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// SeedProjectionWatermarks also lists chats from the artifacts/dag_nodes
	// tables, which normally arrive via NewRowArtifactService's own migrate.
	if _, err := NewRowArtifactService(st.db); err != nil {
		t.Fatalf("NewRowArtifactService: %v", err)
	}
	ls := ledger.NewMemStore()

	chatID := "c1"
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: "turn.started", At: time.Now()}); err != nil {
		t.Fatalf("AppendIntent 1: %v", err)
	}
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: "turn.done", At: time.Now()}); err != nil {
		t.Fatalf("AppendIntent 2: %v", err)
	}
	if err := st.InsertChatEvent(ctx, ChatEvent{ChatID: chatID, Seq: 1, Event: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("InsertChatEvent: %v", err)
	}

	if err := st.SeedProjectionWatermarks(ctx, ls); err != nil {
		t.Fatalf("SeedProjectionWatermarks (first boot): %v", err)
	}
	wm, err := st.GetProjectionWatermark(ctx, chatID, "sse")
	if err != nil {
		t.Fatalf("GetProjectionWatermark: %v", err)
	}
	if wm != 2 {
		t.Fatalf("watermark after first seed = %d; want 2 (ledger's own max seq)", wm)
	}

	// Second boot: more ledger history arrives, but an already-seeded chat
	// must never be touched again (ON CONFLICT DO NOTHING).
	if _, err := ls.AppendIntent(ctx, ledger.Entry{ChatID: chatID, Kind: "turn.started", At: time.Now()}); err != nil {
		t.Fatalf("AppendIntent 3: %v", err)
	}
	if err := st.SeedProjectionWatermarks(ctx, ls); err != nil {
		t.Fatalf("SeedProjectionWatermarks (second boot): %v", err)
	}
	wm, err = st.GetProjectionWatermark(ctx, chatID, "sse")
	if err != nil {
		t.Fatalf("GetProjectionWatermark: %v", err)
	}
	if wm != 2 {
		t.Fatalf("watermark after second seed = %d; want unchanged 2 (no-op re-seed)", wm)
	}
}
