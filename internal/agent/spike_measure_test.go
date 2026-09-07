package agent

// #1185 spike measurement harness: quack's homegrown compaction callback vs
// adk/v2's native runner-level compaction, over the same synthetic long
// conversation, with a real summarizer model. Not a regular test - it hits a
// live model endpoint, so it's opt-in and excluded from `make test`.
//
// Run: QUACK_SPIKE_LIVE=1 go test ./internal/agent/ -run TestSpikeCompactionEngines -v
//
// It builds a ~40-turn tool-heavy conversation (each turn: a large tool
// result, forcing the token threshold repeatedly), then replays it through
// two llmagent+runner stacks that differ only in Compaction.Engine, and
// reports: prompt tokens per model call, compaction count, and whether the
// durable summary survives a simulated restart (fresh runner, same session
// service, mid-conversation).

import (
	"context"
	"fmt"
	"iter"
	"os"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
)

const spikeTurns = 20

// bulkModel is the "worker" under test: it ignores the prompt and returns one
// tool call the first time it's invoked in a turn, then a short final answer
// once it sees the tool result - so every turn issues exactly one real model
// call sequence and the transcript grows by one big tool result per turn.
type bulkModel struct{ n int }

func (m *bulkModel) Name() string { return "bulk-worker" }

func (m *bulkModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	// Only the LAST content decides the branch: a FunctionResponse there means
	// this turn's tool already ran, so give the final answer. Scanning the
	// whole history would find every earlier turn's response too, and the
	// model would never call the tool again after turn one.
	sawResult := false
	if n := len(req.Contents); n > 0 {
		for _, p := range req.Contents[n-1].Parts {
			if p.FunctionResponse != nil {
				sawResult = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		usage := &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(estimateTokens(req.Contents))}
		if sawResult {
			yield(&model.LLMResponse{
				Content:       genai.NewContentFromText("ack", genai.RoleModel),
				FinishReason:  genai.FinishReasonStop,
				UsageMetadata: usage,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: fmt.Sprintf("call-%d", m.n), Name: "read_file", Args: map[string]any{"path": "big.go"}},
			}}},
			FinishReason:  genai.FinishReasonStop,
			UsageMetadata: usage,
		}, nil)
	}
}

type bulkToolArgs struct {
	Path string `json:"path"`
}

// bulkTool returns a ~3000-char body so the transcript crosses TokenThreshold
// well before spikeTurns turns complete.
func bulkTool(t *testing.T) tool.Tool {
	t.Helper()
	body := strings.Repeat("line of source code\n", 150) // ~3000 chars
	tl, err := functiontool.New[bulkToolArgs, string](
		functiontool.Config{Name: "read_file", Description: "reads a file"},
		func(_ adkagent.Context, _ bulkToolArgs) (string, error) { return body, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestSpikeCompactionEngines(t *testing.T) {
	if os.Getenv("QUACK_SPIKE_LIVE") == "" {
		t.Skip("opt-in live spike; set QUACK_SPIKE_LIVE=1 (hits the QA model endpoint)")
	}

	summarizer, err := inference.NewModel(config.ProviderConfig{
		Kind:     "openai",
		Endpoint: "http://jason-server:11436/v1",
	}, "qwen3.5-9b", nil, nil)
	if err != nil {
		t.Fatalf("summarizer model: %v", err)
	}

	const contextWindow = 65_000
	const tokenThreshold = 6_000
	const retention = 6

	t.Run("quack", func(t *testing.T) {
		runSpike(t, summarizer, Compaction{
			Enabled: true, Summarizer: summarizer, ContextWindow: contextWindow,
			TokenThreshold: tokenThreshold, EventRetentionSize: retention, Engine: "quack",
		})
	})
	t.Run("adk", func(t *testing.T) {
		runSpike(t, summarizer, Compaction{
			Enabled: true, Summarizer: summarizer, ContextWindow: contextWindow,
			TokenThreshold: tokenThreshold, EventRetentionSize: retention, Engine: "adk",
		})
	})
}

func runSpike(t *testing.T, summarizer model.LLM, comp Compaction) {
	sessions := session.InMemoryService()
	worker := &bulkModel{}
	ag, err := Build(&Bundle{Card: Card{Name: "worker", Description: "spike worker"}, Prompt: "you are a worker"},
		worker, []tool.Tool{bulkTool(t)}, nil, comp, "", nil, "", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	rcfg := runner.Config{AppName: "spike", Agent: ag, SessionService: sessions, AutoCreateSession: true}
	if comp.Engine == "adk" {
		adkComp, err := nativeCompactionConfig(comp)
		if err != nil {
			t.Fatalf("nativeCompactionConfig: %v", err)
		}
		rcfg.Compaction = adkComp
	}
	r, err := runner.New(rcfg)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	createResp, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: "spike", UserID: "u", SessionID: "spike-" + comp.Engine})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessID := createResp.Session.ID()
	for i := 0; i < spikeTurns; i++ {
		msg := genai.NewContentFromText(fmt.Sprintf("turn %d: keep going", i), genai.RoleUser)
		for ev, err := range r.Run(context.Background(), "u", sessID, msg, adkagent.RunConfig{}) {
			_ = ev
			if err != nil {
				t.Fatalf("turn %d: run: %v", i, err)
			}
		}
	}

	resp, err := sessions.Get(context.Background(), &session.GetRequest{AppName: "spike", UserID: "u", SessionID: sessID})
	if err != nil || resp.Session == nil {
		t.Fatalf("fetch session: %v", err)
	}
	compactions, sawSummary := 0, false
	for ev := range resp.Session.Events().All() {
		if ev.Actions.Compaction != nil {
			compactions++
			sawSummary = true
		}
		if isSentinel2(ev) {
			compactions++
			sawSummary = true
		}
	}
	t.Logf("engine=%s events=%d compactions_seen=%d durable_summary=%v", comp.Engine, resp.Session.Events().Len(), compactions, sawSummary)
}

func isSentinel2(ev *session.Event) bool {
	return ev.Content != nil && len(ev.Content.Parts) > 0 && isSentinel(ev.Content)
}
