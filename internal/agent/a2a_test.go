package agent

import (
	"context"
	"iter"
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

// workerModel is a canned two-turn model: turn 1 emits a thought + a call to the
// echo tool; once it sees the tool's FunctionResponse it emits the final answer.
// It lets the spike exercise the full event vocabulary (thinking / tool_call /
// tool_result / token) across the A2A round-trip with no network or model.
type workerModel struct{}

func (workerModel) Name() string { return "worker-model" }

func (workerModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	sawToolResult := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawToolResult = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if sawToolResult {
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Answer: pong"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{Text: "let me check", Thought: true},
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "echo", Args: map[string]any{"msg": "ping"}}},
			}},
			TurnComplete: true,
		}, nil)
	}
}

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
		Model:       workerModel{},
		Instruction: "Use the echo tool then answer.",
		Tools:       []tool.Tool{echoTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// collect drains a runner stream into the wire-event vocabulary via the stateful
// Translator. The worker here is ungated, so its raw thinking/tool/text parts map
// to agent_thinking / agent_tool_call / agent_tool_result / agent_token.
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

// TestA2ARoundTripPreservesEventVocabulary is the M1 spike for risk #1: it serves
// a worker over real ephemeral-loopback A2A, dispatches to it via the remote
// client, and asserts thinking / tool_call / tool_result / token all survive the
// round-trip (adka2a DataPart metadata ↔ genai parts).
func TestA2ARoundTripPreservesEventVocabulary(t *testing.T) {
	srv, err := Serve(newWorker(t), session.InMemoryService(), nil, Compaction{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node")
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

// transferModel emits a transfer_to_agent call to the named target, then (once it
// sees a tool result) a short wrapper answer. It fakes the orchestrator's
// delegation decision.
type transferModel struct{ target string }

func (transferModel) Name() string { return "transfer-model" }

func (m transferModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	transferred := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "transfer_to_agent" {
				transferred = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if transferred {
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "done"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "t1", Name: "transfer_to_agent", Args: map[string]any{"agent_name": m.target}}},
			}},
			TurnComplete: true,
		}, nil)
	}
}

// TestOrchestratorTransfersToA2ASubAgent is the M1 spike for risk #2: an llmagent
// orchestrator with the A2A client as a sub-agent transfers to it, and the
// sub-agent's events surface through the orchestrator's runner.
func TestOrchestratorTransfersToA2ASubAgent(t *testing.T) {
	srv, err := Serve(newWorker(t), session.InMemoryService(), nil, Compaction{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node")
	if err != nil {
		t.Fatal(err)
	}

	orch, err := llmagent.New(llmagent.Config{
		Name:        "orchestrator",
		Description: "Dispatches to sub-agents.",
		Model:       transferModel{target: client.Name()},
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

// branchCtx fakes the remote agent's InvocationContext for the part converter:
// a plain context that also reports the current run's branch, which is all
// sanitizeWorkflowPlumbingPart reads off it.
type branchCtx struct {
	context.Context
	branch string
}

func (b branchCtx) Branch() string { return b.branch }

// TestSanitizePart_DropsForeignBranchEvents: the converter must drop parts of
// events from a SIBLING node's branch (the shared-session leak: without this,
// remoteagent's history sweep folds a concurrently-running node's prompt and
// plumbing into this node's outbound message), while keeping branchless events,
// the current branch, and ancestors.
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

// TestDescribeEvent_KeepsMediaParts guards the media-reader bug: the gate's
// prompt-delivery event is authored "quack-gate" (foreign), so scopeMessage
// renders it via describeEvent. An attached image rides as an InlineData part
// on that event; describeEvent must carry it across the wire as a raw file
// part, not silently drop it (which left the vision model blind).
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

// TestNativeCompactionConfig covers the #1185 spike's engine switch: "adk"
// builds a runner-level Config from quack's own prompt, anything else (incl.
// the zero Compaction) leaves native compaction off.
func TestNativeCompactionConfig(t *testing.T) {
	base := Compaction{Enabled: true, Summarizer: workerModel{}, ContextWindow: 65_000, TokenThreshold: 40_000, EventRetentionSize: 20}

	if cfg, err := nativeCompactionConfig(Compaction{}); err != nil || cfg != nil {
		t.Fatalf("disabled: got (%v, %v), want (nil, nil)", cfg, err)
	}
	quackEngine := base
	quackEngine.Engine = "quack"
	if cfg, err := nativeCompactionConfig(quackEngine); err != nil || cfg != nil {
		t.Fatalf("engine=quack: got (%v, %v), want (nil, nil)", cfg, err)
	}

	adkEngine := base
	adkEngine.Engine = "adk"
	cfg, err := nativeCompactionConfig(adkEngine)
	if err != nil {
		t.Fatalf("engine=adk: %v", err)
	}
	if cfg == nil || cfg.TokenThreshold != 40_000 || cfg.EventRetentionSize != 20 || cfg.Summarizer == nil {
		t.Fatalf("engine=adk: got %+v, want quack's thresholds carried over with a summarizer set", cfg)
	}

	noSummarizer := adkEngine
	noSummarizer.Summarizer = nil
	if _, err := nativeCompactionConfig(noSummarizer); err == nil {
		t.Fatal("engine=adk with no summarizer: want an error, got nil")
	}
}
