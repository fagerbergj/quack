package dag_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// citeStub fetches twice, then answers citing the first page; as judge it passes and counts rounds.
type citeStub struct {
	mu     sync.Mutex
	judged int
	calls  int
}

func (*citeStub) Name() string { return "citeStub" }

func (s *citeStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if atHasTool(req, "submit_verdict") {
			s.judged++
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.calls++
		if s.calls <= 2 {
			yield(atCall("web_fetch", map[string]any{"urls": []any{fmt.Sprintf("https://example.com/p%d", s.calls)}}), nil)
			return
		}
		yield(atText("The finding, per [the source](https://example.com/p1), holds; it is detailed enough to clear the gate's length floor easily."), nil)
	}
}

// citeFetchTool returns a page per call; the second call blocks until its round is aborted.
func citeFetchTool(t *testing.T, started chan<- struct{}) tool.Tool {
	t.Helper()
	var mu sync.Mutex
	n := 0
	tl, err := functiontool.New[map[string]any, map[string]any](
		functiontool.Config{Name: "web_fetch", Description: "fetches pages"},
		func(ctx adkagent.Context, args map[string]any) (map[string]any, error) {
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			if k == 2 {
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			u := args["urls"].([]any)[0].(string)
			return map[string]any{"results": []any{map[string]any{"url": u, "content": "page"}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// A continued researcher cites a page it fetched before the pause. The resumed run's gate session holds none of
// that work, so the judge must credit it from the saved worker session, or it fails the round for "no retrieval"
// and sends the worker to re-research in a fresh revise session.
func TestContinue_JudgeCreditsTheInterruptedAttemptsFetches(t *testing.T) {
	testNativeWorkerSessions = session.InMemoryService()
	stub := &citeStub{}
	started := make(chan struct{}, 1)
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo",
		Tools: []tool.Tool{citeFetchTool(t, started)}})
	if err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Research the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": testNativeWorker{Agent: worker}}, nil,
		vetting.NewJudgeFactory(stub, nil, nil),
		func(context.Context, string) vetting.Config {
			return vetting.Config{Threshold: 0.6, JudgeRounds: 2, RequireRetrieval: true}
		}, nil)
	go func() {
		<-started
		ex.PauseNode("chat1", "n1", dag.PauseUser)
	}()
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content,
		func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out := retryInWorkflow(t, ex, plan, "chat1", "n1", true); !strings.Contains(out["n1"], "The finding") {
		t.Fatalf("continued output = %q", out["n1"])
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	// Two fetch calls and the answer; a fourth worker call is a revise round the judge should not have needed.
	if stub.calls != 3 || stub.judged != 1 {
		t.Errorf("worker calls = %d, judge rounds = %d; want 3 and 1 (no revise)", stub.calls, stub.judged)
	}
}
