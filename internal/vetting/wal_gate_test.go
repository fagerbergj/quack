package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// fakeGateLedger: in-memory WAL with a gapless per-chat seq. failKind/failOccurrence fail the Nth
// AppendIntent of a kind once; one node's WAL calls are single-goroutine, so counting is deterministic.
type fakeGateLedger struct {
	mu             sync.Mutex
	seqs           map[string]int64
	entries        []ledger.Entry
	failKind       string
	failOccurrence int // 1-indexed; 0 = disabled
	seenOfKind     int
}

func newFakeGateLedger() *fakeGateLedger {
	return &fakeGateLedger{seqs: map[string]int64{}}
}

func (f *fakeGateLedger) List(context.Context) ([]ledger.SessionRef, error) { return nil, nil }
func (f *fakeGateLedger) Delete(context.Context, string) error              { return nil }

func (f *fakeGateLedger) MaxSeq(_ context.Context, chatID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seqs[chatID], nil
}

// entryMatchesFailKind: the literal ledger kind, or for "judge_round" an artifact.revision whose
// payload names that kind.
func entryMatchesFailKind(e ledger.Entry, failKind string) bool {
	if e.Kind == failKind {
		return true
	}
	if failKind == "judge_round" && e.Kind == ledger.KindArtifactRevision {
		var p struct {
			Kind string `json:"kind"`
		}
		return json.Unmarshal(e.Payload, &p) == nil && p.Kind == "judge_round"
	}
	return false
}

func (f *fakeGateLedger) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKind != "" && entryMatchesFailKind(e, f.failKind) {
		f.seenOfKind++
		if f.seenOfKind == f.failOccurrence {
			return 0, errors.New("fakeGateLedger: forced AppendIntent failure")
		}
	}
	f.seqs[e.ChatID]++
	e.Seq = f.seqs[e.ChatID]
	f.entries = append(f.entries, e)
	return e.Seq, nil
}

func (f *fakeGateLedger) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledger.Entry
	for _, e := range f.entries {
		if e.ChatID == chatID && e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeGateLedger) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.entries))
	for i, e := range f.entries {
		out[i] = e.Kind
	}
	return out
}

// A fail-then-pass node's WAL sees node.started, each round's artifact.revision writes, then
// node.done, in that order and nothing else.
func TestGatedNodeWALEntryOrder(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	fl := newFakeGateLedger()
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
		// Artifact (not IsReviewer) writes a per-round document without the reviewer VERDICT-tag
		// check, which stubModel's plain answers don't satisfy.
		Artifact: kindText, ChatID: "chat1", User: "u1",
		Artifacts: artifact.InMemoryService(), Ledger: fl,
	}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	kinds := fl.kinds()
	if len(kinds) == 0 {
		t.Fatal("no WAL entries recorded")
	}
	if kinds[0] != ledger.KindNodeStarted {
		t.Fatalf("first entry kind = %q, want %q", kinds[0], ledger.KindNodeStarted)
	}
	if last := kinds[len(kinds)-1]; last != ledger.KindNodeDone && last != ledger.KindNodeFailed {
		t.Fatalf("last entry kind = %q, want node.done or node.failed", last)
	}
	// Between start and close every entry is an artifact.revision; two rounds means exactly two
	// carry a judge_round payload.
	var judgeRounds int
	for _, e := range fl.entries[1 : len(fl.entries)-1] {
		if e.Kind != ledger.KindArtifactRevision {
			t.Fatalf("unexpected WAL entry kind in the middle of the run: %q", e.Kind)
		}
		var p struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Kind == "judge_round" {
			judgeRounds++
		}
	}
	if judgeRounds != 2 {
		t.Fatalf("judge_round artifact.revision entries = %d, want 2 (fail then pass)", judgeRounds)
	}
}

// With no Ledger, there are no WAL calls and the round loop doesn't require one.
func TestGatedNodeNoLedgerConfiguredNoWALCalls(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	var final string
	for ev, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil && ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p != nil && strings.TrimSpace(p.Text) != "" {
					final = p.Text
				}
			}
		}
	}
	if !strings.Contains(final, "revised") {
		t.Fatalf("expected the gate to still converge with no ledger configured, got %q", final)
	}
}

// node.started/node.done are best-effort: a failed node.started append doesn't affect the run,
// and node.done still lands.
func TestGatedNodeNodeEventAppendFailureIsBestEffort(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	fl := newFakeGateLedger()
	fl.failKind = ledger.KindNodeStarted
	fl.failOccurrence = 1
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
		Artifact: kindText, ChatID: "chat1", User: "u1",
		Artifacts: artifact.InMemoryService(), Ledger: fl,
	}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	var final string
	for ev, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil && ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p != nil && strings.TrimSpace(p.Text) != "" {
					final = p.Text
				}
			}
		}
	}
	if !strings.Contains(final, "revised") {
		t.Fatalf("a node.started append failure should not affect the run, got %q", final)
	}
	kinds := fl.kinds()
	if len(kinds) == 0 {
		t.Fatal("no WAL entries recorded")
	}
	// The forced-to-fail node.started never lands, but node.done still must.
	for _, k := range kinds {
		if k == ledger.KindNodeStarted {
			t.Fatal("node.started should have failed to append and not appear in the log")
		}
	}
	if last := kinds[len(kinds)-1]; last != ledger.KindNodeDone && last != ledger.KindNodeFailed {
		t.Fatalf("last entry kind = %q, want node.done or node.failed even though node.started's append failed", last)
	}
}

// The judge_round WAL write is fail-closed: failing it on a passing round stops the loop with
// Passed=false, names the failure in Feedback, and starts no further revise.
func TestGatedNodeJudgeRoundAppendFailureStopsOnPassingRound(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	fl := newFakeGateLedger()
	// Fail the second judge_round append (the passing round), so a real pass is forced closed.
	fl.failKind = "judge_round"
	fl.failOccurrence = 2
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
		Artifact: kindText, ChatID: "chat1", User: "u1",
		Artifacts: artifact.InMemoryService(), Ledger: fl,
	}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if res.Passed {
		t.Fatal("res.Passed should be forced false when the passing round's judge.round WAL append fails")
	}
	if !strings.Contains(res.Feedback, "write-ahead log") {
		t.Fatalf("res.Feedback = %q, want it to name the WAL append failure", res.Feedback)
	}
	// Draft + one revise = 2 worker calls; a round-3 revise would make it 3.
	if stub.workerCalls != 2 {
		t.Fatalf("worker calls = %d, want 2 (no revise round after the WAL-forced failure)", stub.workerCalls)
	}
}
