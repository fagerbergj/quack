package tools

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ── shared stub-model helpers (mirrors internal/dag's gCall/gText/gSysText -
// duplicated here because internal/tools already imports internal/dag, so a
// dag-package test file can't import tools back without a cycle) ───────────

func atText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// atAllText concatenates every non-thought text part across the request's
// contents (the running conversation, INCLUDING tool call/response parts'
// adjacent text) - used to assert what a stub model actually saw.
func atAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// ── harness: a gated dynamic node running a real worker AgentNode (with the
// real ask_advisor tool attached) inside a minimal one-node graph, mirroring
// the shape dag.newGatedNode builds in production (gated node → RunNode'd
// worker): the advisor-thread marker in the prompt + the registered
// task/rubric are how the tool resolves its thread and seed. ────────────────

// runAdvisorHarness runs one turn: a worker (using workerModel) calling into
// the REAL ask_advisor tool bound to an advisor (using advisorModel) and the
// given session.Service. task/rubric are registered under the node's
// advisor-thread token and the marker stamped into the prompt, exactly as
// dag.newGatedNode does. Returns the worker's final answer text.
func runAdvisorHarness(t *testing.T, workerModel, advisorModel model.LLM, sessions session.Service, nodeID, task, rubric string) string {
	t.Helper()

	advisorAgent, err := llmagent.New(llmagent.Config{
		Name: "advisor", Model: advisorModel, Description: "advisor", Instruction: "Advise.",
		// ModeChat pinned, as production does via agent.BuildChat: the runner
		// would otherwise force-set it with an unsynchronized write on this
		// SHARED instance - a data race under concurrent consults.
		Mode: llmagent.ModeChat,
	})
	if err != nil {
		t.Fatalf("advisor agent: %v", err)
	}
	tl, err := NewAskAdvisorTool(advisorAgent, sessions)
	if err != nil {
		t.Fatalf("NewAskAdvisorTool: %v", err)
	}
	workerAgent, err := llmagent.New(llmagent.Config{
		Name: "worker", Model: workerModel, Description: "worker", Instruction: "Answer.",
		Tools: []tool.Tool{tl},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	workerNode, err := workflow.NewAgentNode(workerAgent, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("worker node: %v", err)
	}

	gated := workflow.NewDynamicNode[any, string](nodeID,
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			token := vetting.AdvisorThreadToken("p", nodeID)
			vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{Task: task, Rubric: rubric})
			defer vetting.UnregisterAdvisorThread(token)
			prompt := vetting.AdvisorThreadMarker(token) + "\n\ngo"
			return workflow.RunNode[string](ctx, workerNode, prompt,
				workflow.WithUseSubBranch(), workflow.WithRunID("worker-r0"))
		}, workflow.NodeConfig{})

	eb := workflow.NewEdgeBuilder()
	eb.Add(workflow.Start, gated)
	wfAgent, err := workflowagent.New(workflowagent.Config{Name: "wrap", Edges: eb.Build()})
	if err != nil {
		t.Fatalf("workflow agent: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "quack-test", Agent: wfAgent, SessionService: sessions, AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	var out strings.Builder
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	for ev, rerr := range r.Run(context.Background(), "u", "s", content, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	return out.String()
}

// ── Test 1: mentor memory within one draft ──────────────────────────────────

// twoConsultWorker calls ask_advisor twice (distinct requests) before writing
// its final answer - proving the worker CAN consult repeatedly in one draft.
type twoConsultWorker struct {
	mu    sync.Mutex
	calls int
}

func (*twoConsultWorker) Name() string { return "twoConsultWorker" }

func (s *twoConsultWorker) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		switch n {
		case 1:
			yield(atCall("ask_advisor", map[string]any{"request": "REQUEST-ONE: is my approach sound?"}), nil)
		case 2:
			yield(atCall("ask_advisor", map[string]any{"request": "REQUEST-TWO: anything else before I write up?"}), nil)
		default:
			yield(atText("final worker answer"), nil)
		}
	}
}

// recordingAdvisor answers every consult with a fixed reply per call number,
// recording the full prompt text (INCLUDING prior session history) it saw on
// each call - the assertion surface for native session memory.
type recordingAdvisor struct {
	mu       sync.Mutex
	prompts  []string
	replyFor func(callNum int) string
}

func (*recordingAdvisor) Name() string { return "recordingAdvisor" }

func (s *recordingAdvisor) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.prompts = append(s.prompts, atAllText(req))
		n := len(s.prompts)
		s.mu.Unlock()
		reply := "ADVICE"
		if s.replyFor != nil {
			reply = s.replyFor(n)
		}
		yield(atText(reply), nil)
	}
}

// TestAskAdvisor_MentorMemoryWithinOneDraft: a worker consults ask_advisor
// twice in one draft. The advisor's SECOND LLM request must carry both the
// first request text AND the advisor's own first reply - native ADK session
// memory (a persistent session + a plain runner.Run, unlike agenttool's
// cold-session-per-call - see NewAskAdvisorTool's doc comment).
func TestAskAdvisor_MentorMemoryWithinOneDraft(t *testing.T) {
	advisor := &recordingAdvisor{replyFor: func(n int) string {
		if n == 1 {
			return "ADVICE-ONE"
		}
		return "ADVICE-TWO"
	}}
	sessions := session.InMemoryService()
	answer := runAdvisorHarness(t, &twoConsultWorker{}, advisor, sessions, "n1", "the task", "the rubric")
	if answer != "final worker answer" {
		t.Fatalf("worker answer = %q, want completion despite two consults", answer)
	}

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.prompts) != 2 {
		t.Fatalf("advisor called %d times, want 2", len(advisor.prompts))
	}
	second := advisor.prompts[1]
	if !strings.Contains(second, "REQUEST-ONE") {
		t.Errorf("advisor's 2nd request missing the 1st request text; got:\n%s", second)
	}
	if !strings.Contains(second, "ADVICE-ONE") {
		t.Errorf("advisor's 2nd request missing its OWN 1st reply (no native memory); got:\n%s", second)
	}
	if !strings.Contains(second, "REQUEST-TWO") {
		t.Errorf("advisor's 2nd request missing the 2nd request text; got:\n%s", second)
	}
}

// ── Test 2: seeded outcome ──────────────────────────────────────────────────

// oneConsultWorker calls ask_advisor exactly once then answers.
type oneConsultWorker struct {
	mu    sync.Mutex
	calls int
}

func (*oneConsultWorker) Name() string { return "oneConsultWorker" }

func (s *oneConsultWorker) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(atCall("ask_advisor", map[string]any{"request": "should I proceed this way?"}), nil)
			return
		}
		yield(atText("final answer"), nil)
	}
}

// TestAskAdvisor_SeededWithTaskAndRubric: the advisor's FIRST LLM request
// (on a brand-new per-node session) must contain the node's task + acceptance
// rubric - seeded from session state written by dag.newGatedNode (mirrored
// here by the harness) via dag.NodeTaskStateKey/NodeRubricStateKey - so the
// mentor knows the desired outcome from its very first reply.
func TestAskAdvisor_SeededWithTaskAndRubric(t *testing.T) {
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	runAdvisorHarness(t, &oneConsultWorker{}, advisor, sessions, "n1",
		"TASK-SENTINEL: research the thing", "RUBRIC-SENTINEL: must cite two sources")

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.prompts) != 1 {
		t.Fatalf("advisor called %d times, want 1", len(advisor.prompts))
	}
	first := advisor.prompts[0]
	if !strings.Contains(first, "TASK-SENTINEL") {
		t.Errorf("advisor's 1st request missing the node's task; got:\n%s", first)
	}
	if !strings.Contains(first, "RUBRIC-SENTINEL") {
		t.Errorf("advisor's 1st request missing the node's acceptance rubric; got:\n%s", first)
	}
}

// usageAdvisor replies with a fixed usage-carrying response - drives the
// SetDefaultAgent metric test without recordingAdvisor's prompt bookkeeping.
type usageAdvisor struct{}

func (usageAdvisor) Name() string { return "usage-advisor" }

func (usageAdvisor) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ADVICE"}}},
			FinishReason:  genai.FinishReasonStop,
			TurnComplete:  true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
		}, nil)
	}
}

// TestAskAdvisor_DefaultAgentFillsTokenUsage pins serve.go's advisor wiring:
// ask_advisor runs the advisor via its own nested runner.Run, whose ctx never
// carries the worker's node coords, so the advisor's tracedModel needs the
// SetDefaultAgent("advisor") fallback to attribute its token usage at all.
func TestAskAdvisor_DefaultAgentFillsTokenUsage(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}

	advisor := inference.TracedModelForTesting(usageAdvisor{}, "advisor-test-model")
	if da, ok := advisor.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent("advisor")
	} else {
		t.Fatal("TracedModelForTesting result does not implement SetDefaultAgent")
	}
	sessions := session.InMemoryService()
	runAdvisorHarness(t, &oneConsultWorker{}, advisor, sessions, "n1", "task", "rubric")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "gen_ai.client.token.usage" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
			}
			for _, dp := range sum.DataPoints {
				agentVal, _ := dp.Attributes.Value(attribute.Key("agent"))
				if agentVal.AsString() == "advisor" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no gen_ai.client.token.usage data point carries agent=advisor - the advisor's SetDefaultAgent fallback never reached the metric")
	}
}

// ── Test 5: advisor error → empty advice, worker completes normally ────────

// brokenAdvisorSessions wraps a real session.Service but fails every
// Get/Create scoped to the advisor's own AppName - simulating a broken
// advisor session store while the MAIN workflow session (a different
// AppName) keeps working normally, so this exercises consultAdvisor's OWN
// error handling (runner.Run failing against the store) rather than
// anything upstream of it.
type brokenAdvisorSessions struct {
	session.Service
}

func (b brokenAdvisorSessions) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req.AppName == advisorAppName {
		return nil, errors.New("boom: advisor store unavailable")
	}
	return b.Service.Get(ctx, req)
}

func (b brokenAdvisorSessions) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req.AppName == advisorAppName {
		return nil, errors.New("boom: advisor store unavailable")
	}
	return b.Service.Create(ctx, req)
}

// adviceCapturingWorker calls ask_advisor once, records the advice it got
// back (empty on a failed consult), then answers unconditionally - proving
// the worker is never blocked by an advisor failure.
type adviceCapturingWorker struct {
	mu     sync.Mutex
	calls  int
	advice string
	seen   bool
}

func (*adviceCapturingWorker) Name() string { return "adviceCapturingWorker" }

func (s *adviceCapturingWorker) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(atCall("ask_advisor", map[string]any{"request": "any advice?"}), nil)
			return
		}
		// Round 2: the tool's FunctionResponse (with the "advice" field) is now
		// in the request history - extract it so the test can assert it's empty.
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "ask_advisor" {
					s.mu.Lock()
					s.seen = true
					if adv, ok := p.FunctionResponse.Response["advice"].(string); ok {
						s.advice = adv
					}
					s.mu.Unlock()
				}
			}
		}
		yield(atText("worker finished anyway"), nil)
	}
}

// TestAskAdvisor_RunnerErrorYieldsEmptyAdvice: a broken session store makes
// consultAdvisor's isolated runner error. The tool must swallow that error
// (best-effort), return empty advice, log a warning, and - critically - never
// fail or block the calling worker.
func TestAskAdvisor_RunnerErrorYieldsEmptyAdvice(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	advisor := &recordingAdvisor{}
	worker := &adviceCapturingWorker{}
	sessions := brokenAdvisorSessions{Service: session.InMemoryService()}
	answer := runAdvisorHarness(t, worker, advisor, sessions, "n1", "task", "rubric")

	if answer != "worker finished anyway" {
		t.Fatalf("worker answer = %q, want the worker to complete despite the advisor error", answer)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !worker.seen {
		t.Fatal("worker never saw an ask_advisor tool response")
	}
	if worker.advice != "" {
		t.Errorf("advice = %q, want empty on advisor runner error", worker.advice)
	}
	if !strings.Contains(logBuf.String(), "ask_advisor") {
		t.Errorf("expected a warning logged for the failed consult; log:\n%s", logBuf.String())
	}
}
