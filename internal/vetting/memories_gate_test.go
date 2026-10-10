// memories_gate_test.go: a judge that skips the required memory votes gets the in-session nudge;
// a passed round logs what happened.
package vetting

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
)

// memoryVoteJudge is a worker+judge stub: its first submit_verdict skips memories, and the nudge turn
// either votes on every owed id or repeats the omission.
func memoryVoteJudge(voteOnRetry bool) fnLLM {
	var calls int32
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if !stubHasTool(req, submitVerdictTool) {
			return stubText("the answer"), nil
		}
		if atomic.AddInt32(&calls, 1) == 1 || !voteOnRetry {
			return stubCall(submitVerdictTool, map[string]any{"score": 0.95, "feedback": "ok"}), nil
		}
		// Read the owed ids from the nudge text rather than hardcoding what the test seeded.
		text := stubAllText(req)
		start := strings.Index(text, "Vote on memories ")
		var mems []any
		if start >= 0 {
			rest := text[start+len("Vote on memories "):]
			if end := strings.Index(rest, " via"); end >= 0 {
				for _, id := range strings.Split(rest[:end], ", ") {
					mems = append(mems, map[string]any{"id": id, "vote": "supported", "reason": "confirmed by the diff"})
				}
			}
		}
		return stubCall(submitVerdictTool, map[string]any{"score": 0.95, "feedback": "ok", "memories": mems}), nil
	}
}

// runMemoryVoteNode seeds one recallable memory matching MemoryScope, runs one gated node with
// ExternalWorker+CommitMemory so prefill recall fires for real, and returns the evidence.
func runMemoryVoteNode(t *testing.T, voteOnRetry bool) (res GateResult, lgr *ledgertest.MemStore, logs string) {
	t.Helper()
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_votes_gate", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	scope := memory.Scope{Role: "coding", Legacy: "n1"}
	if _, err := store.Commit(ctx, scope, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "always run go test before committing"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}

	lgr = ledgertest.NewMemStore()
	judge := memoryVoteJudge(voteOnRetry)
	worker, err := llmagent.New(llmagent.Config{Name: "n1", Model: judge, Description: "worker", Instruction: "answer"})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		ChatID: "chat1", Agent: "worker", NodeID: "n1", JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10",
		ExternalWorker: true, CommitMemory: true, Memory: store, MemoryRole: "coding", Task: "run tests", Ledger: lgr,
	}
	node, err := newTestGatedNodeCapture("n1", worker, judge, NewJudgeFactory(judge, nil, nil), cfg, &res)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "do the task"}}}
	for ev, err := range r.Run(t.Context(), "u", "s-n1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = ev
	}
	return res, lgr, buf.String()
}

// A judge that skips memories on its first submit_verdict is nudged, votes on the retry, and the
// passed round applies and logs the votes.
func TestRunGatedRefine_MemoryVotesNudge_AppliesOnRetry(t *testing.T) {
	res, lgr, logs := runMemoryVoteNode(t, true)
	if !res.Passed {
		t.Fatalf("expected the gate to pass, got %+v", res)
	}
	entries, err := lgr.ReadEntries(context.Background(), "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var votes int
	for _, e := range entries {
		if e.Kind == ledger.KindMemoryVote {
			votes++
		}
	}
	if votes != 1 {
		t.Fatalf("memory.vote entries = %d, want 1 (the nudge should have recovered the vote)", votes)
	}
	if !strings.Contains(logs, "memory votes applied") {
		t.Errorf("expected an info log for the applied vote, got: %s", logs)
	}
}

// A judge that ignores the nudge too still passes, but zero votes land and the round logs a warning
// instead of silently dropping the received memories.
func TestRunGatedRefine_MemoryVotesNudge_WarnsWhenStillMissing(t *testing.T) {
	res, lgr, logs := runMemoryVoteNode(t, false)
	if !res.Passed {
		t.Fatalf("expected the gate to pass, got %+v", res)
	}
	entries, err := lgr.ReadEntries(context.Background(), "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	for _, e := range entries {
		if e.Kind == ledger.KindMemoryVote {
			t.Fatalf("expected no memory.vote entries, got one: %+v", e)
		}
	}
	if !strings.Contains(logs, "left some unvoted after the nudge") {
		t.Errorf("expected a warning about unvoted memories after the nudge, got: %s", logs)
	}
}
