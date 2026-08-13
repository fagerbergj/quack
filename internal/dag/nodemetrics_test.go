package dag

import (
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// A finished node must report its cost: duration and what the trust gate said.
// Regression: duration_ms was never assigned in nodeDoneData (structurally
// always 0); judge fields were read back via a fresh sessions.Get before the
// gated node's state delta had appended, so the read saw nothing; started_at
// was nulled by the node_done upsert (store.UpsertDagNode).
func TestNodeDoneReportsDurationAndGateResult(t *testing.T) {
	const node = "explorer-goose"

	ds := newDagStream(
		map[string]string{node: "code-explorer"},
		func(stream.SSEEvent, error) bool { return true },
		map[string]string{node: "goose registers tools via ExtensionManager…"},
		func(id string) gateScore { return gateScore{score: 1.0, passed: true, rounds: 1} },
		func(string) bool { return false },
		func(string) bool { return false },
		func(string, int) string { return "" },
	)

	// The node ran: mark it started (this is what emitting node_start does).
	ds.started[node] = true
	ds.startedAt[node] = nowMinus(t, 90) // 90s ago

	d := ds.nodeDoneData(node)

	if d.DurationMs <= 0 {
		t.Errorf("node_done reports duration_ms=%d - a completed node must say how long it took", d.DurationMs)
	}
	if d.DurationMs < 89_000 {
		t.Errorf("duration_ms=%d, want ~90s", d.DurationMs)
	}
	if d.JudgeFinalScore != 1.0 || !d.JudgePassed || d.JudgeRounds != 1 {
		t.Errorf("node_done lost the judge result: score=%v passed=%v rounds=%d - the node claims its trust gate never ran",
			d.JudgeFinalScore, d.JudgePassed, d.JudgeRounds)
	}
}

// The in-memory gate result is what node_done actually reads: the session-state write
// isn't visible to a fresh Get at that moment. Recording it must round-trip.
func TestRecordedGateResultIsReadableImmediately(t *testing.T) {
	e := &Executor{}
	e.recordGateResult("chat-1", "n1", 0.85, true, 2)

	got := e.gateScore(t.Context(), "quack", "local", "chat-1", "n1")
	if got.score != 0.85 || !got.passed || got.rounds != 2 {
		t.Fatalf("gateScore = %+v, want {0.85 true 2} - node_done cannot see the gate's own result", got)
	}

	// A different chat with the same node id must not collide.
	if other := e.gateScore(t.Context(), "quack", "local", "chat-2", "n1"); other.score != 0 {
		t.Fatalf("gate result leaked across chats: %+v", other)
	}
}

// Regression: node_done reported zero tokens - nodeDoneData read the per-run
// usage accumulator, which closeRun always nils before node_done is built.
// Token usage (and cached, added alongside) must be a cumulative total across
// every worker/revise round, not just the last one closed.
func TestNodeDoneReportsCumulativeTokenUsage(t *testing.T) {
	const r0 = "quack-dag-p@1/n1@rr/web-researcher@worker-r0"
	const r1 = "quack-dag-p@1/n1@rr/web-researcher@worker-r1"
	const npath = "quack-dag-p@1/n1@rr"
	agentByID := map[string]string{"n1": "web-researcher"}

	withUsage := func(path string, prompt, completion, total, cached int32, model, text string) *session.Event {
		e := ev(path, &genai.Part{Text: text})
		e.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: prompt, CandidatesTokenCount: completion, TotalTokenCount: total, CachedContentTokenCount: cached,
		}
		e.ModelVersion = model
		return e
	}

	// r0 and r1 deliberately use different models, so nd.Model below actually
	// proves last-run-wins semantics rather than two rounds agreeing by coincidence.
	evs := []*session.Event{
		withUsage(r0, 100, 20, 120, 10, "qwen3", "draft"),
		withUsage(r1, 50, 15, 65, 40, "claude-sonnet", "revised"),
		{NodeInfo: &session.NodeInfo{Path: npath}, Output: "final"},
	}

	got := drive(evs, agentByID, gateScore{})
	var nd stream.NodeDoneData
	complete := map[string]stream.AgentCompleteData{}
	for _, e := range got {
		switch e.Name {
		case stream.EventNodeDone:
			nd = e.Data.(stream.NodeDoneData)
		case stream.EventAgentComplete:
			d := e.Data.(stream.AgentCompleteData)
			complete[d.RunID] = d
		}
	}
	if nd.PromptTokens != 150 || nd.CompletionTokens != 35 || nd.TotalTokens != 185 || nd.CachedTokens != 50 {
		t.Fatalf("node_done usage = %+v, want cumulative prompt=150 completion=35 total=185 cached=50", nd)
	}
	if nd.Model != "claude-sonnet" {
		t.Fatalf("node_done model = %q, want claude-sonnet (last run wins)", nd.Model)
	}

	r0Done, r1Done := complete["worker-r0"], complete["worker-r1"]
	if r0Done.PromptTokens != 100 || r0Done.CompletionTokens != 20 || r0Done.CachedTokens != 10 {
		t.Errorf("worker-r0 agent_complete usage = %+v, want per-run prompt=100 completion=20 cached=10", r0Done)
	}
	if r1Done.PromptTokens != 50 || r1Done.CompletionTokens != 15 || r1Done.CachedTokens != 40 {
		t.Errorf("worker-r1 agent_complete usage = %+v, want per-run prompt=50 completion=15 cached=40", r1Done)
	}
}

// ContextTokens must be the LAST measured prompt-token count, not summed like
// PromptTokens - a multi-tool-call round's calls each report the model's
// growing context size, so summing them overshoots what the model actually
// held at any one time.
func TestAgentCompleteContextTokensIsLastNotSummed(t *testing.T) {
	const r0 = "quack-dag-p@1/n1@rr/web-researcher@worker-r0"
	const r1 = "quack-dag-p@1/n1@rr/web-researcher@worker-r1"
	const npath = "quack-dag-p@1/n1@rr"
	agentByID := map[string]string{"n1": "web-researcher"}

	withPrompt := func(path string, prompt int32) *session.Event {
		e := ev(path, &genai.Part{Text: "x"})
		e.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: prompt}
		return e
	}

	evs := []*session.Event{
		// r0: two tool-call round trips within the same round - context grows
		// with each call, so the round's occupancy is the LAST one (60k), not
		// the sum (100k).
		withPrompt(r0, 40_000),
		withPrompt(r0, 60_000),
		// r1: one call.
		withPrompt(r1, 90_000),
		{NodeInfo: &session.NodeInfo{Path: npath}, Output: "final"},
	}

	got := drive(evs, agentByID, gateScore{})
	var nd stream.NodeDoneData
	complete := map[string]stream.AgentCompleteData{}
	for _, e := range got {
		switch e.Name {
		case stream.EventNodeDone:
			nd = e.Data.(stream.NodeDoneData)
		case stream.EventAgentComplete:
			d := e.Data.(stream.AgentCompleteData)
			complete[d.RunID] = d
		}
	}
	if got := complete["worker-r0"].ContextTokens; got != 60_000 {
		t.Errorf("worker-r0 ContextTokens = %d, want 60000 (last call, not the 100000 sum)", got)
	}
	if got := complete["worker-r1"].ContextTokens; got != 90_000 {
		t.Errorf("worker-r1 ContextTokens = %d, want 90000", got)
	}
	// node_done also overwrites across rounds rather than summing.
	if nd.ContextTokens != 90_000 {
		t.Errorf("node_done ContextTokens = %d, want 90000 (freshest round, not 40000+60000+90000)", nd.ContextTokens)
	}
}

// nowMinus returns a time n seconds in the past.
func nowMinus(t *testing.T, secs int) time.Time {
	t.Helper()
	return time.Now().Add(-time.Duration(secs) * time.Second)
}
