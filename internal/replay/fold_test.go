package replay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

func appendOtelLine(t *testing.T, s ledger.LedgerStore, chatID string, e entry) int64 {
	t.Helper()
	l := line{Timestamp: e.ts, Attrs: e.attrs}
	payload, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}
	seq, err := s.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, Kind: otelEntryKind, At: e.ts, Payload: payload,
	})
	if err != nil {
		t.Fatalf("AppendIntent: %v", err)
	}
	return seq
}

// TestFoldToSeq_StopsAtBoundary: entries after seqN must not appear in the
// folded session - the point of "fold to seq N and continue live" is that a
// caller can EnableFork exactly there.
func TestFoldToSeq_StopsAtBoundary(t *testing.T) {
	fs, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seq1 := appendOtelLine(t, fs, chatID, chat(base, "n1", "worker", "0", "model-a", map[string]any{
		"gen_ai.output.messages": `[{"role":"model","parts":[{"text":"first"}]}]`,
	}))
	appendOtelLine(t, fs, chatID, chat(base.Add(time.Second), "n2", "worker", "0", "model-b", map[string]any{
		"gen_ai.output.messages": `[{"role":"model","parts":[{"text":"second"}]}]`,
	}))

	sess, err := FoldToSeq(context.Background(), fs, chatID, seq1)
	if err != nil {
		t.Fatalf("FoldToSeq: %v", err)
	}
	if _, ok := sess.streams[StreamKey{Node: "n1", Agent: "worker", Round: "0"}]; !ok {
		t.Fatalf("stream n1 missing from folded session")
	}
	if _, ok := sess.streams[StreamKey{Node: "n2", Agent: "worker", Round: "0"}]; ok {
		t.Fatalf("stream n2 present in a fold that stopped before it")
	}
}

// TestFoldToSeq_FullLog matches a fold to the log's last seq against every entry.
func TestFoldToSeq_FullLog(t *testing.T) {
	fs, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	const chatID = "chat-1"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	appendOtelLine(t, fs, chatID, chat(base, "n1", "worker", "0", "model-a", nil))
	last := appendOtelLine(t, fs, chatID, chat(base.Add(time.Second), "n2", "worker", "0", "model-b", nil))

	sess, err := FoldToSeq(context.Background(), fs, chatID, last)
	if err != nil {
		t.Fatalf("FoldToSeq: %v", err)
	}
	if len(sess.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(sess.streams))
	}
}
