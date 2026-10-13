package dag_test

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

// roundStub answers every worker call, recording how many contents and how
// many model-authored turns each request carried, and the text of its first message.
type roundStub struct {
	mu        sync.Mutex
	contents  []int
	modelTurn []int
	first     []string
}

func (*roundStub) Name() string { return "roundStub" }

func (s *roundStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		turns := 0
		for _, c := range req.Contents {
			if c != nil && c.Role == genai.RoleModel {
				turns++
			}
		}
		s.contents = append(s.contents, len(req.Contents))
		var first []string
		if len(req.Contents) > 0 {
			for _, p := range req.Contents[0].Parts {
				first = append(first, p.Text)
			}
		}
		s.first = append(s.first, strings.Join(first, "\n"))
		s.modelTurn = append(s.modelTurn, turns)
		n := len(s.contents)
		s.mu.Unlock()
		yield(atText(strings.Repeat("draft ", 5)+string(rune('A'+n))), nil)
	}
}

// countingCreates counts the worker-side A2A sessions a node creates.
type countingCreates struct {
	session.Service
	mu  sync.Mutex
	ids []string
}

func (c *countingCreates) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	resp, err := c.Service.Create(ctx, req)
	if err == nil && resp != nil && resp.Session != nil {
		c.mu.Lock()
		c.ids = append(c.ids, resp.Session.ID())
		c.mu.Unlock()
	}
	return resp, err
}

// TestReviseRound_StartsAFreshWorkerSession: each round runs on its own sub-branch, so scopeMessage
// clears the A2A context id and a revise round's worker session holds only the revise prompt.
func TestReviseRound_StartsAFreshWorkerSession(t *testing.T) {
	stub := &roundStub{}
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	workerSessions := &countingCreates{Service: session.InMemoryService()}
	srv, err := quackagent.Serve(worker, workerSessions, nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", quackagent.WorkerSessionID("c1", "n1"))
	if err != nil {
		t.Fatal(err)
	}

	sessions := session.InMemoryService()
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
		vetting.NewJudgeFactory(failOnceJudge(0.1, "add more detail"), nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.contents) != 2 {
		t.Fatalf("worker calls = %d (%v), want 2: the draft and one revise round", len(stub.contents), stub.contents)
	}
	if stub.modelTurn[1] != 0 || stub.contents[1] != 1 {
		t.Errorf("revise round request carried %d contents, %d of them model turns; want 1 and 0 (a fresh session with only the revise prompt)", stub.contents[1], stub.modelTurn[1])
	}
	workerSessions.mu.Lock()
	defer workerSessions.mu.Unlock()
	if len(workerSessions.ids) != 2 || workerSessions.ids[0] != quackagent.WorkerSessionID("c1", "n1") {
		t.Errorf("worker sessions created = %v, want the node's deterministic one for the draft and a fresh one for the revise round", workerSessions.ids)
	}
}

// TestReviseRound_FirstMessageExtendsTheDraftsMessage: the draft's first message must be a byte prefix of the
// revise round's, so the revise reuses the draft's cached prefix; the plan step's trigger used to lead only the draft's.
func TestReviseRound_FirstMessageExtendsTheDraftsMessage(t *testing.T) {
	stub := &roundStub{}
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := quackagent.Serve(worker, session.InMemoryService(), nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", quackagent.WorkerSessionID("c1", "n1"))
	if err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p1", UserMessage: "Summarise the reviews.", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"solo": client}, nil,
		vetting.NewJudgeFactory(failOnceJudge(0.1, "add more detail"), nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "run"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.first) != 2 {
		t.Fatalf("worker calls = %d, want the draft and one revise round", len(stub.first))
	}
	draft, revise := stub.first[0], stub.first[1]
	if !strings.Contains(draft, "Write the thing.") || !strings.HasPrefix(revise, draft) {
		t.Errorf("revise round's first message diverges from the draft's at char %d:\ndraft:  %q\nrevise: %q", sharedPrefix(draft, revise), draft, revise)
	}
}

func sharedPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}
