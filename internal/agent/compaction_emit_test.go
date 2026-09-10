package agent

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// TestEmitCompactionReachesHub unit-tests emitCompaction's own translation (field mapping, span attributes) against a hand-built Event; the proof that
// adk's real compactor ever calls compactionSessions.AppendEvent with one is TestCompactionSessionsObservesRealCompaction in a2a_test.go. It also proves the compaction row's run_id is quack's own run id - the same one the node's
// agent_start event carries (both derive from the branch via stream.RunIDFromBranch) - not adk's invocation id, so the frontend can match by exact run_id instead of a "most recent run on this node" heuristic.
func TestEmitCompactionReachesHub(t *testing.T) {
	hub := stream.NewHub()
	const chatID, nodeID = "chat-1", "node-B"
	const branch = "worker@worker-r2"
	var seq int64
	sink := func(ev stream.SSEEvent) {
		seq++
		hub.Publish(chatID, seq, ev)
	}

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	ev := &session.Event{InvocationID: "inv-1", Branch: branch}
	ev.Actions.Compaction = &session.EventCompaction{
		StartTimestamp:   start,
		EndTimestamp:     end,
		CompactedContent: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "summary"}}},
	}
	ev.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 900, CandidatesTokenCount: 40}

	emitCompaction(context.Background(), sink, nodeID, ev)

	replay, _, cancel, _ := hub.Subscribe(chatID)
	defer cancel()
	if len(replay) != 1 {
		t.Fatalf("hub replay = %d events, want 1", len(replay))
	}
	got, ok := replay[0].SSE.Data.(stream.CompactionData)
	if !ok {
		t.Fatalf("event data = %T, want stream.CompactionData", replay[0].SSE.Data)
	}
	if replay[0].SSE.Name != stream.EventCompaction {
		t.Errorf("event name = %q, want %q", replay[0].SSE.Name, stream.EventCompaction)
	}
	if got.NodeID != nodeID {
		t.Errorf("NodeID = %q, want %q", got.NodeID, nodeID)
	}
	// Same round/node's agent_start would carry this exact run id (see
	// dag.segRun, the other caller of stream.RunIDFromBranch) - never adk's
	// own invocation id ("inv-1").
	wantRunID := stream.RunIDFromBranch(branch)
	if got.RunID != wantRunID {
		t.Errorf("RunID = %q, want the round's agent_start run id %q", got.RunID, wantRunID)
	}
	if got.RunID == ev.InvocationID {
		t.Fatalf("RunID must not be adk's own invocation id %q", ev.InvocationID)
	}
	if got.SummaryInputTokens != 900 || got.SummaryOutputTokens != 40 {
		t.Errorf("tokens = %d/%d, want 900/40", got.SummaryInputTokens, got.SummaryOutputTokens)
	}
}

// TestEmitCompactionNilSinkNoop guards the "no active hub" no-op path
// (compaction disabled callers, or a test with no run in flight) - it must
// never panic on a nil sink.
func TestEmitCompactionNilSinkNoop(t *testing.T) {
	ev := &session.Event{}
	ev.Actions.Compaction = &session.EventCompaction{}
	emitCompaction(context.Background(), nil, "node-A", ev)
}

// TestEmitCompactionSkipsNonCompactionEvent guards against a false-positive
// row on every ordinary event this runner observes.
func TestEmitCompactionSkipsNonCompactionEvent(t *testing.T) {
	called := false
	emitCompaction(context.Background(), func(stream.SSEEvent) { called = true }, "node-A", &session.Event{})
	if called {
		t.Fatal("emitCompaction called sink for an event with no Actions.Compaction")
	}
}
