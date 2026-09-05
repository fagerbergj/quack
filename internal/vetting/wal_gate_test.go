package vetting

import (
	"context"
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

// fakeGateLedger is an in-memory ledger.LedgerStore double for the gate's own
// WAL hooks (#1100): AppendIntent allocates a gapless per-chat seq, in the
// order calls arrive - enough to assert node/judge entry ordering without a
// real Postgres container. failKind/failOccurrence (mirrors the recordstore
// test's fakeLedger.failNext, but per-kind and by occurrence count since a
// gated round appends several kinds) force the Nth AppendIntent call of
// failKind to fail, then clear themselves - RunGatedRefine drives one node's
// WAL calls from a single goroutine, so counting occurrences synchronously
// here is deterministic, no timing games needed.
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

func (f *fakeGateLedger) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKind != "" && e.Kind == f.failKind {
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

// TestGatedNodeWALEntryOrder is #1100 test case (c): for one gated round that
// fails then passes (stubModel's usual shape), the WAL sees node.started,
// this round's artifact.revision writes, judge.round, the next round's
// artifact.revision writes, judge.round, node.done - in that order, and
// nothing else.
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
		// Artifact (not IsReviewer) triggers the per-round document write
		// without the reviewer-specific VERDICT-tag completion check, which
		// stubModel's plain-text answers don't satisfy.
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
	// Between node.started and the close, every artifact.revision run for a
	// round must precede that round's judge.round (stubModel fails round 1,
	// passes round 2 - so exactly 2 judge.round entries).
	var judgeRounds int
	sawRevisionSinceLastJudge := false
	for _, k := range kinds[1 : len(kinds)-1] {
		switch k {
		case ledger.KindArtifactRevision:
			sawRevisionSinceLastJudge = true
		case ledger.KindJudgeRound:
			if !sawRevisionSinceLastJudge {
				t.Fatal("judge.round entry with no preceding artifact.revision entry this round")
			}
			sawRevisionSinceLastJudge = false
			judgeRounds++
		default:
			t.Fatalf("unexpected WAL entry kind in the middle of the run: %q", k)
		}
	}
	if judgeRounds != 2 {
		t.Fatalf("judge.round entries = %d, want 2 (fail then pass)", judgeRounds)
	}
}

// TestGatedNodeNoLedgerConfiguredNoWALCalls is #1100 test case (d): a Config
// with no Ledger set must behave exactly as before #1100 - no WAL calls, and
// the round loop must not itself require one.
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

// TestGatedNodeNodeEventAppendFailureIsBestEffort is #1100 review case (a):
// node.started/node.done are best-effort - a forced AppendIntent failure on
// node.started must not affect the run, and node.done must still land at
// the end.
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

// TestGatedNodeJudgeRoundAppendFailureStopsOnPassingRound is #1100 review
// case (b): appendJudgeRound is fail-closed - a forced failure on a PASSING
// round must stop the round loop, force res.Passed=false, name the WAL
// failure in res.Feedback, and never start another revise round (asserted
// via the worker's own call count).
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
	// stubModel fails round 1, passes round 2 - fail the SECOND judge.round
	// append (the passing one), so a real "would have passed" verdict is the
	// one forced closed. AppendIntent counts occurrences synchronously, so
	// arming this before the run is enough - no timing games needed.
	fl.failKind = ledger.KindJudgeRound
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
	// Round 1's draft + revise = 2 worker calls; the forced failure on round
	// 2's judge.round append must stop the loop before any round-3 revise,
	// so worker calls must stay at 2 (not 3).
	if stub.workerCalls != 2 {
		t.Fatalf("worker calls = %d, want 2 (no revise round after the WAL-forced failure)", stub.workerCalls)
	}
}
