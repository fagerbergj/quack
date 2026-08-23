package dag_test

// Concurrent plan nodes that share ONE agent must each get their OWN task over
// the A2A hop: ADK's remote-agent scans the shared workflow session backward
// for the first event authored by the agent's name and reuses that event's
// A2A session, so same-agent siblings can adopt each other's remote task.

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// repoStub is the REMOTE worker's model: it reports which repos it can see in
// the request the remote server built for it, so the test asserts on what the
// remote side actually received (not merely that the node completed).
type repoStub struct {
	mu   sync.Mutex
	seen [][]string // repos visible per worker request, in call order
}

func (*repoStub) Name() string { return "repoStub" }

func (s *repoStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var repos []string
		txt := atAllText(req)
		for _, r := range []string{"ALPHA", "BETA"} {
			if strings.Contains(txt, r) {
				repos = append(repos, r)
			}
		}
		s.mu.Lock()
		s.seen = append(s.seen, repos)
		s.mu.Unlock()
		yield(atText("FINAL: cloned "+strings.Join(repos, "+")), nil)
	}
}

// passJudge always passes, so the only thing under test is which task reached
// the remote worker.
type passJudge struct{}

func (passJudge) Name() string { return "passJudge" }
func (passJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// synthStub terminates the plan (ADK allows one terminal node) without touching
// the repos under test.
type synthStub struct{}

func (synthStub) Name() string { return "synthStub" }
func (synthStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(atText("SUMMARY"), nil)
	}
}

// TestConcurrentNodes_SameAgentOverA2A_KeepTheirOwnTask: two CONCURRENT nodes of
// the SAME A2A agent, each with a different task. Each remote dispatch must
// carry exactly its own node's task - no sibling's repo, and never an empty
// task ("Which repository would you like me to explore?" in the live run).
func TestConcurrentNodes_SameAgentOverA2A_KeepTheirOwnTask(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &repoStub{}

	// ONE agent, ONE A2A server, ONE client - exactly the production shape
	// (internal/serve builds one client per agent name and hands it to the DAG).
	worker, err := llmagent.New(llmagent.Config{
		Name: "explorer", Model: stub, Description: "explorer", Instruction: "ROLE:explorer Explore the repo you were given.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}
	synth, err := llmagent.New(llmagent.Config{
		Name: "synth", Model: synthStub{}, Description: "synth", Instruction: "ROLE:synth Summarize.",
	})
	if err != nil {
		t.Fatalf("synth agent: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "explore the two repos", Nodes: []dag.Node{
		{ID: "n1", AgentName: "explorer", Task: "Clone and explore repo ALPHA.", Rubric: "covers ALPHA"},
		{ID: "n2", AgentName: "explorer", Task: "Clone and explore repo BETA.", Rubric: "covers BETA"},
		{ID: "synth", AgentName: "synth", Task: "Summarize both.", Rubric: "covers both", DependsOn: []string{"n1", "n2"}},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"explorer": client, "synth": synth}, nil,
		vetting.NewJudgeFactory(passJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "explore the two repos"}}}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if paused {
		t.Fatal("run should not pause")
	}

	// The remote worker's answer names the repos IT saw. Crossed tasks show up
	// as "BETA" under n1, "ALPHA+BETA" (both nodes in one remote session), or an
	// empty list (this node's prompt truncated out of the outbound message).
	if got := outputs["n1"]; !strings.Contains(got, "ALPHA") || strings.Contains(got, "BETA") {
		t.Errorf("n1 (ALPHA) output = %q - the remote worker did not get n1's own task", got)
	}
	if got := outputs["n2"]; !strings.Contains(got, "BETA") || strings.Contains(got, "ALPHA") {
		t.Errorf("n2 (BETA) output = %q - the remote worker did not get n2's own task", got)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for i, seen := range stub.seen {
		if len(seen) != 1 {
			t.Errorf("remote worker request %d saw repos %v - want exactly one (its own node's task)", i, seen)
		}
	}
}

// reviseStub drafts once, then (after the judge's fail) revises. Its second
// draft asserts the remote session CONTINUED: the revise round's request must
// still carry the first draft, which only happens when the node resumed the SAME
// remote A2A contextID (multi-turn dispatch). A per-node identity must keep this
// working - unique across nodes, but stable across a node's rounds.
type reviseStub struct {
	mu       sync.Mutex
	calls    int
	sawDraft bool // the revise request carried the first draft back
}

func (*reviseStub) Name() string { return "reviseStub" }

func (s *reviseStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		if n > 1 && strings.Contains(atAllText(req), "DRAFT-ONE") {
			s.sawDraft = true
		}
		s.mu.Unlock()
		if n == 1 {
			yield(atText("DRAFT-ONE"), nil)
			return
		}
		yield(atText("FINAL-TWO"), nil)
	}
}

// failThenPassJudge fails the first verdict (forcing a revise round) and passes
// the second.
type failThenPassJudge struct {
	mu     sync.Mutex
	rounds int
}

func (*failThenPassJudge) Name() string { return "failThenPassJudge" }
func (j *failThenPassJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		j.mu.Lock()
		j.rounds++
		n := j.rounds
		j.mu.Unlock()
		if n == 1 {
			yield(atCall("submit_verdict", map[string]any{"score": 0.2, "feedback": "add detail"}), nil)
			return
		}
		yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// TestNodeOverA2A_ResumesItsOwnRemoteSessionAcrossRounds: the per-node identity
// must stay STABLE across a node's judge/revise rounds - the revise dispatch has
// to land back in the node's own remote A2A session (carrying the draft it is
// revising), not a fresh one.
func TestNodeOverA2A_ResumesItsOwnRemoteSessionAcrossRounds(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &reviseStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
		vetting.NewJudgeFactory(&failThenPassJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := outputs["n1"]; !strings.Contains(got, "FINAL-TWO") {
		t.Fatalf("n1 output = %q, want the revised answer", got)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.calls < 2 {
		t.Fatalf("worker ran %d times, want a draft + a revise round", stub.calls)
	}
	if !stub.sawDraft {
		t.Error("the revise round did not see the node's own first draft - the node did not resume its own remote A2A session")
	}
}
