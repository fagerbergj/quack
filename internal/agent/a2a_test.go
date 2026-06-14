package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
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
		func(_ adkagent.ToolContext, a echoArgs) (string, error) { return "pong:" + a.Msg, nil },
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
	srv, err := Serve(newWorker(t), session.InMemoryService())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.Client()
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
	srv, err := Serve(newWorker(t), session.InMemoryService())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.Client()
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
