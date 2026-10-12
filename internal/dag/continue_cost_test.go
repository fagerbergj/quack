package dag_test

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// costStub plays a researcher: one fetch per call until `fetches` have come back, then an answer.
// It reports each request's char-estimated size as its prompt usage, as a real provider would.
type costStub struct {
	fetches int
	ok      atomic.Int32 // fetches that came back; compaction hides them from the request, so the tool counts
	mu      sync.Mutex
	worker  []int // prompt tokens of each worker request
}

func (*costStub) Name() string { return "costStub" }

func reqTokens(req *model.LLMRequest) int {
	chars := 0
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			chars += len(p.Text)
			if p.FunctionCall != nil {
				b, _ := json.Marshal(p.FunctionCall.Args)
				chars += len(b)
			}
			if p.FunctionResponse != nil {
				b, _ := json.Marshal(p.FunctionResponse.Response)
				chars += len(b)
			}
		}
	}
	return chars / 4
}

func (s *costStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		tokens := reqTokens(req)
		var resp *model.LLMResponse
		s.mu.Lock()
		switch {
		case !atHasTool(req, "fetch"): // the compaction summarizer
			resp = atText("SUMMARY of the work so far.")
		case int(s.ok.Load()) < s.fetches:
			s.worker = append(s.worker, tokens)
			resp = atCall("fetch", map[string]any{"n": s.ok.Load()})
		default:
			s.worker = append(s.worker, tokens)
			resp = atText("FINAL ANSWER")
		}
		s.mu.Unlock()
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(tokens), CandidatesTokenCount: 10, TotalTokenCount: int32(tokens) + 10}
		yield(resp, nil)
	}
}

// pagingTool returns a ~1k-token page; call number blockAt (1-based, 0 = never) blocks until its round is aborted.
func pagingTool(t *testing.T, ok *atomic.Int32, blockAt int, started chan<- struct{}) tool.Tool {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	tl, err := functiontool.New[map[string]any, map[string]any](
		functiontool.Config{Name: "fetch", Description: "fetches a page"},
		func(ctx adkagent.Context, _ map[string]any) (map[string]any, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == blockAt {
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ok.Add(1)
			return map[string]any{"body": strings.Repeat("page text ", 400)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// costThreshold: the compaction threshold the cost runs use, in the stub's char/4 tokens (a page is ~1k).
const costThreshold = 6000

// costRun runs n1 once (pausing it at fetch blockAt when > 0, then continuing it) and returns each phase's
// worker request sizes and the prompt tokens the first run's node_done reported.
func costRun(t *testing.T, fetches, blockAt int) (first, continued []int, reported int32) {
	t.Helper()
	testNativeWorkerSessions = session.InMemoryService()
	stub := &costStub{fetches: fetches}
	started := make(chan struct{}, 1)
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo",
		Tools: []tool.Tool{pagingTool(t, &stub.ok, blockAt, started)}})
	if err != nil {
		t.Fatal(err)
	}
	comp := quackagent.Compaction{Enabled: true, Summarizer: stub, TokenThreshold: costThreshold, EventRetentionSize: 4}
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Research the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": testNativeWorker{Agent: worker, comp: comp}}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	if blockAt > 0 {
		go func() {
			<-started
			ex.PauseNode("chat1", "n1", dag.PauseUser)
		}()
	}
	sink := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.NodeDoneData); ok {
			reported = d.PromptTokens
		}
		return true
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content, sink, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	stub.mu.Lock()
	first, stub.worker = stub.worker, nil
	stub.mu.Unlock()
	if blockAt > 0 {
		retryInWorkflow(t, ex, plan, "chat1", "n1", true)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return first, stub.worker, reported
}

// A continued node pays for its history on every call, but no more than a run that was never paused: compaction
// still bounds each request, the repaired session adds no duplicate history, and node_done counts each call once.
func TestContinue_RequestsStayBoundedLikeAnUninterruptedRun(t *testing.T) {
	const fetches, pauseAt = 16, 9
	fresh, _, reported := costRun(t, fetches, 0)
	pre, cont, _ := costRun(t, fetches, pauseAt)
	t.Logf("uninterrupted: %d calls, %d prompt tokens %v", len(fresh), sum(fresh), fresh)
	t.Logf("paused+continued: %d+%d calls, %d+%d prompt tokens %v %v", len(pre), len(cont), sum(pre), sum(cont), pre, cont)
	if int(reported) != sum(fresh) {
		t.Errorf("node_done prompt_tokens = %d, the model was sent %d", reported, sum(fresh))
	}
	for i, n := range cont {
		if n > costThreshold+1500 {
			t.Errorf("continued call %d sent %d tokens, over the %d compaction threshold plus a page", i, n, costThreshold)
		}
	}
	if total := sum(pre) + sum(cont); total*4 > sum(fresh)*5 {
		t.Errorf("paused+continued sent %d tokens, more than 1.25x the uninterrupted run's %d", total, sum(fresh))
	}
}
