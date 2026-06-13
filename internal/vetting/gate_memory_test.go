package vetting

import (
	"context"
	"testing"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/stream"
)

// fakeCommitter records every Commit call so tests can assert exactly when the
// gate writes to memory.
type fakeCommitter struct {
	reqs []memory.CommitRequest
	err  error
}

func (f *fakeCommitter) Commit(_ context.Context, r memory.CommitRequest) error {
	f.reqs = append(f.reqs, r)
	return f.err
}

// runGateCommit runs one gated turn with a committer and returns it plus the
// memory_commit events that reached the stream.
func runGateCommit(t *testing.T, workerModel, judge model.LLM, cfg Config, fc memory.Committer) (*fakeCommitter, []stream.MemoryCommitData) {
	t.Helper()
	committer, _ := fc.(*fakeCommitter)
	worker, err := llmagent.New(llmagent.Config{
		Name:        "web-researcher",
		Description: "researches the web",
		Model:       workerModel,
		Instruction: "research",
	})
	if err != nil {
		t.Fatal(err)
	}
	gated, err := NewGatedAgent(worker, judge, cfg, fc)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "web-researcher",
		Agent:             gated,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "what is up"}}}

	var commits []stream.MemoryCommitData
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
		for _, se := range stream.Translate(ev) {
			if d, ok := se.Data.(stream.MemoryCommitData); ok {
				commits = append(commits, d)
			}
		}
	}
	return committer, commits
}

func TestGateCommitsOnPass(t *testing.T) {
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.9,"passed":true,"feedback":"good"}`}}
	fc := &fakeCommitter{}

	got, commits := runGateCommit(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"}, fc)

	if len(got.reqs) != 1 {
		t.Fatalf("commit calls = %d, want 1", len(got.reqs))
	}
	req := got.reqs[0]
	if req.AppName != "web-researcher" || req.Agent != "web-researcher" {
		t.Errorf("commit appName/agent = %q/%q, want web-researcher", req.AppName, req.Agent)
	}
	if req.UserID != "u" {
		t.Errorf("commit userID = %q, want u", req.UserID)
	}
	if req.Query != "what is up" {
		t.Errorf("commit query = %q, want %q", req.Query, "what is up")
	}
	if req.Finding == "" {
		t.Errorf("commit finding is empty")
	}
	if req.Score != 0.9 {
		t.Errorf("commit score = %v, want 0.9", req.Score)
	}
	if len(commits) != 1 {
		t.Fatalf("memory_commit events = %d, want 1", len(commits))
	}
	if !floatEq(commits[0].Score, 0.9) {
		t.Errorf("memory_commit score = %v, want 0.9", commits[0].Score)
	}
}

func TestGateNoCommitWhenJudgeNeverPasses(t *testing.T) {
	// Always-failing scores: the loop exhausts MaxRounds and breaks without a
	// pass, so reaching the seam must NOT commit.
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.2,"passed":false,"feedback":"weak"}`}}
	fc := &fakeCommitter{}

	got, commits := runGateCommit(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"}, fc)

	if len(got.reqs) != 0 {
		t.Errorf("commit calls = %d, want 0 (never passed)", len(got.reqs))
	}
	if len(commits) != 0 {
		t.Errorf("memory_commit events = %d, want 0", len(commits))
	}
}

func TestGateNoCommitWhenJudgeUnavailable(t *testing.T) {
	// The judge errors → judge_unavailable early return, which never reaches the
	// commit seam.
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	fc := &fakeCommitter{}

	got, commits := runGateCommit(t, worker, erroringModel{name: "j"}, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"}, fc)

	if len(got.reqs) != 0 {
		t.Errorf("commit calls = %d, want 0 (judge unavailable)", len(got.reqs))
	}
	if len(commits) != 0 {
		t.Errorf("memory_commit events = %d, want 0", len(commits))
	}
}

func TestGateCommitErrorDoesNotBlockAnswer(t *testing.T) {
	// A failing committer must be swallowed: no memory_commit event, but the run
	// still completes (no panic / error propagation).
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.9,"passed":true,"feedback":"good"}`}}
	fc := &fakeCommitter{err: context.DeadlineExceeded}

	got, commits := runGateCommit(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"}, fc)

	if len(got.reqs) != 1 {
		t.Errorf("commit attempts = %d, want 1", len(got.reqs))
	}
	if len(commits) != 0 {
		t.Errorf("memory_commit events = %d, want 0 on commit error", len(commits))
	}
}

func floatEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}
