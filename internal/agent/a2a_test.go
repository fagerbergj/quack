package agent

import (
	"context"
	"iter"
	"slices"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// fakeLLM answers each call with one response computed from the request.
type fakeLLM struct {
	gen func(*model.LLMRequest) *model.LLMResponse
}

func (*fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(f.gen(req), nil) }
}

// turn is one complete model turn made of parts.
func turn(parts ...*genai.Part) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: parts}, TurnComplete: true}
}

// funcResponses lists every FunctionResponse in req.
func funcResponses(req *model.LLMRequest) []*genai.FunctionResponse {
	var out []*genai.FunctionResponse
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				out = append(out, p.FunctionResponse)
			}
		}
	}
	return out
}

// workerModel thinks and calls echo, then answers once it sees the tool result, covering the full event vocabulary.
var workerModel = &fakeLLM{func(req *model.LLMRequest) *model.LLMResponse {
	if len(funcResponses(req)) > 0 {
		return turn(&genai.Part{Text: "Answer: pong"})
	}
	return turn(&genai.Part{Text: "let me check", Thought: true},
		&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "echo", Args: map[string]any{"msg": "ping"}}})
}}

type echoArgs struct {
	Msg string `json:"msg"`
}

func echoTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[echoArgs, string](
		functiontool.Config{Name: "echo", Description: "Echo the message back."},
		func(_ adkagent.Context, a echoArgs) (string, error) { return "pong:" + a.Msg, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func newWorker(t *testing.T) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "spike-worker",
		Description: "A test worker agent.",
		Model:       workerModel,
		Instruction: "Use the echo tool then answer.",
		Tools:       []tool.Tool{echoTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// collect drains a runner stream through the Translator; the worker is ungated, so its parts map to
// agent_thinking / agent_tool_call / agent_tool_result / agent_token.
func collect(t *testing.T, seq iter.Seq2[*session.Event, error]) (thinking, answer string, toolCalls, toolResults []string) {
	t.Helper()
	tr := stream.NewTranslator()
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		for _, se := range tr.Event(ev) {
			switch d := se.Data.(type) {
			case stream.AgentThinkingData:
				thinking += d.Text
			case stream.AgentTokenData:
				answer += d.Text
			case stream.AgentToolCallData:
				toolCalls = append(toolCalls, d.Name)
			case stream.AgentToolResultData:
				toolResults = append(toolResults, d.Name)
			}
		}
	}
	return
}

// Thinking, tool_call, tool_result and token must all survive a real loopback A2A round-trip.
func TestA2ARoundTripPreservesEventVocabulary(t *testing.T) {
	srv, err := Serve(newWorker(t), session.InMemoryService(), nil, nil, Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "spike",
		Agent:             client,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "say something"}}}
	thinking, answer, calls, results := collect(t, r.Run(context.Background(), "local", "s1", content, adkagent.RunConfig{}))

	if !strings.Contains(thinking, "let me check") {
		t.Errorf("thinking = %q, want it to contain %q", thinking, "let me check")
	}
	if len(calls) == 0 || calls[0] != "echo" {
		t.Errorf("tool calls = %v, want [echo ...]", calls)
	}
	if len(results) == 0 || results[0] != "echo" {
		t.Errorf("tool results = %v, want [echo ...]", results)
	}
	if !strings.Contains(answer, "pong") {
		t.Errorf("answer = %q, want it to contain %q", answer, "pong")
	}
}

// transferModel fakes the orchestrator's delegation: transfer to target, then answer once transferred.
func transferModel(target string) *fakeLLM {
	return &fakeLLM{func(req *model.LLMRequest) *model.LLMResponse {
		if slices.ContainsFunc(funcResponses(req), func(fr *genai.FunctionResponse) bool { return fr.Name == "transfer_to_agent" }) {
			return turn(&genai.Part{Text: "done"})
		}
		return turn(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "t1", Name: "transfer_to_agent", Args: map[string]any{"agent_name": target}}})
	}}
}

// An orchestrator with the A2A client as a sub-agent transfers to it, and the sub-agent's events surface
// through the orchestrator's runner.
func TestOrchestratorTransfersToA2ASubAgent(t *testing.T) {
	srv, err := Serve(newWorker(t), session.InMemoryService(), nil, nil, Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatal(err)
	}

	orch, err := llmagent.New(llmagent.Config{
		Name:        "orchestrator",
		Description: "Dispatches to sub-agents.",
		Model:       transferModel(client.Name()),
		Instruction: "Delegate to the worker.",
		SubAgents:   []adkagent.Agent{client},
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "spike",
		Agent:             orch,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "research this"}}}
	_, answer, calls, results := collect(t, r.Run(context.Background(), "local", "s1", content, adkagent.RunConfig{}))

	// The worker's echo tool_call/tool_result must surface through the transfer.
	if len(calls) == 0 {
		t.Errorf("expected the sub-agent's tool calls to surface, got none (calls=%v results=%v)", calls, results)
	}
	if !strings.Contains(answer, "pong") {
		t.Errorf("answer = %q, want it to contain the worker's %q", answer, "pong")
	}
}

// branchCtx is a context reporting the run's branch, all sanitizeWorkflowPlumbingPart reads off it.
type branchCtx struct {
	context.Context
	branch string
}

func (b branchCtx) Branch() string { return b.branch }

// The converter drops a sibling branch's events (else remoteagent's history sweep leaks them into this node's message),
// keeping branchless events, the current branch, and ancestors.
func TestSanitizePart_DropsForeignBranchEvents(t *testing.T) {
	cur := "n1@1.researcher@worker-r0"
	textPart := &genai.Part{Text: "some content"}
	cases := []struct {
		name     string
		ctx      context.Context
		evBranch string
		want     bool // want a non-nil converted part
	}{
		{"sibling node dropped", branchCtx{context.Background(), cur}, "n2@1", false},
		{"sibling worker dropped", branchCtx{context.Background(), cur}, "n2@1.researcher@worker-r0", false},
		{"own earlier run dropped", branchCtx{branch: cur, Context: context.Background()}, "n1@1.researcher@worker-r1", false},
		{"prefix without dot boundary dropped", branchCtx{context.Background(), "n1@10.researcher@worker-r0"}, "n1@1", false},
		{"branchless kept", branchCtx{context.Background(), cur}, "", true},
		{"exact branch kept", branchCtx{context.Background(), cur}, cur, true},
		{"ancestor kept", branchCtx{context.Background(), cur}, "n1@1", true},
		{"unbranched invocation keeps all", branchCtx{context.Background(), ""}, "n2@1", true},
		{"plain context keeps all", context.Background(), "n2@1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &session.Event{Branch: tc.evBranch}
			ev.Author = "quack-gate"
			got, err := sanitizeWorkflowPlumbingPart(tc.ctx, ev, textPart)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if (got != nil) != tc.want {
				t.Errorf("converted part present = %v, want %v", got != nil, tc.want)
			}
		})
	}
}

// The gate's prompt event is foreign-authored, so describeEvent renders it; an attached image must cross the
// wire as a file part rather than be dropped, which left the vision model blind.
func TestDescribeEvent_KeepsMediaParts(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG magic
	ev := &session.Event{}
	ev.Author = "quack-gate"
	ev.Content = &genai.Content{Role: "user", Parts: []*genai.Part{
		{Text: "Your task: describe the attached image."},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: imgBytes}},
	}}

	parts := describeEvent(ev)

	var gotImage bool
	for _, p := range parts {
		if raw := p.Raw(); raw != nil {
			gotImage = true
			if string(raw) != string(imgBytes) {
				t.Errorf("image bytes mangled: got %v want %v", raw, imgBytes)
			}
			if p.MediaType != "image/png" {
				t.Errorf("image media type = %q, want image/png", p.MediaType)
			}
		}
	}
	if !gotImage {
		t.Fatal("describeEvent dropped the image part - the vision model never sees it")
	}
}

// Disabled leaves native compaction off; enabled builds it from quack's prompt and thresholds.
func TestNativeCompactionConfig(t *testing.T) {
	base := Compaction{Enabled: true, Summarizer: workerModel, ContextWindow: 65_000, TokenThreshold: 40_000, EventRetentionSize: 20}

	if cfg, err := NativeCompactionConfig(Compaction{}); err != nil || cfg != nil {
		t.Fatalf("disabled: got (%v, %v), want (nil, nil)", cfg, err)
	}

	cfg, err := NativeCompactionConfig(base)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if cfg == nil || cfg.TokenThreshold != 40_000 || cfg.EventRetentionSize != 20 || cfg.Summarizer == nil {
		t.Fatalf("enabled: got %+v, want quack's thresholds carried over with a summarizer set", cfg)
	}

	noSummarizer := base
	noSummarizer.Summarizer = nil
	if _, err := NativeCompactionConfig(noSummarizer); err == nil {
		t.Fatal("no summarizer: want an error, got nil")
	}
}

// textModel always answers with text and no tool calls, so one Run is one complete turn.
func textModel(text string) *fakeLLM {
	return &fakeLLM{func(*model.LLMRequest) *model.LLMResponse { return turn(&genai.Part{Text: text}) }}
}

// compactionSessions must catch a compaction adk's own runner fires: with CompactionInterval:1 the real
// sliding-window compactor appends after one complete invocation.
func TestCompactionSessionsObservesRealCompaction(t *testing.T) {
	ag, err := llmagent.New(llmagent.Config{
		Name:        "compaction-worker",
		Description: "A test worker agent.",
		Model:       textModel("Answer: 42"),
		Instruction: "Answer directly.",
	})
	if err != nil {
		t.Fatal(err)
	}

	adkComp, err := NativeCompactionConfig(Compaction{
		Enabled:            true,
		Summarizer:         textModel("the user asked a question"),
		CompactionInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []stream.SSEEvent
	sink := func(ev stream.SSEEvent) { events = append(events, ev) }
	sessions := compactionSessions{Service: session.InMemoryService(), nodeID: "node-real", sink: sink}

	r, err := runner.New(runner.Config{
		AppName:           "compaction-e2e",
		Agent:             ag,
		SessionService:    sessions,
		AutoCreateSession: true,
		Compaction:        adkComp,
	})
	if err != nil {
		t.Fatal(err)
	}

	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}
	for ev, err := range r.Run(context.Background(), "local", "s1", content, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = ev
	}

	if len(events) == 0 {
		t.Fatal("compactionSessions never observed a compaction event from adk's real compactor")
	}
	data, ok := events[0].Data.(stream.CompactionData)
	if !ok {
		t.Fatalf("event data = %T, want stream.CompactionData", events[0].Data)
	}
	if data.NodeID != "node-real" {
		t.Errorf("NodeID = %q, want %q", data.NodeID, "node-real")
	}
	if events[0].Name != stream.EventCompaction {
		t.Errorf("event name = %q, want %q", events[0].Name, stream.EventCompaction)
	}
}

// TestMaxTranscriptChars pins that adk's summarizer cap is derived from the
// configured context window rather than left at adk's 200k-char default.
func TestMaxTranscriptChars(t *testing.T) {
	if got, want := maxTranscriptChars(Compaction{ContextWindow: 65_000}), 65_000*charsPerToken; got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if got := maxTranscriptChars(Compaction{}); got != 0 {
		t.Fatalf("unknown window: got %d, want 0 (keeps adk's default)", got)
	}
}
