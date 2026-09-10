// nativecontinue_test.go proves the ADK-native half of the continue
// primitive (dag/graph.go's sessionNodeID, threaded through
// nativeAgent.ForNode to internal/agent's A2A executor): a continuing
// node's own LlmRequest must contain the prior node's events, and a fresh
// sibling's must not.
package serve

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/agent"
)

// captureModel records every LlmRequest's contents text - the assertion
// surface for "did this node's model call see the prior node's events".
type captureModel struct {
	mu    sync.Mutex
	texts []string
}

func (m *captureModel) Name() string { return "capture" }

func (m *captureModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
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
		m.mu.Lock()
		m.texts = append(m.texts, b.String())
		m.mu.Unlock()
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "ok"}}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (m *captureModel) allText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.texts, "\n")
}

// runNativeWorkerTurn mirrors exactly what internal/agent.Serve's A2A
// executor does internally (adka2a.ExecutorConfig.RunnerConfig): AppName is
// the agent's own name, Mode is left unset by agent.Build (production's
// worker construction, internal/serve/serve.go's buildWorker) so
// runner.Run auto-forces it to ModeChat (internal/agent.BuildChat's doc
// comment) - the SAME mechanism this test exercises directly, without the
// A2A HTTP transport itself (third-party, unchanged by this fix).
func runNativeWorkerTurn(t *testing.T, bundle *agent.Bundle, sessions session.Service, m model.LLM, sessionID, text string) {
	t.Helper()
	wag, err := agent.Build(bundle, m, nil, nil, "", nil, "", nil)
	if err != nil {
		t.Fatalf("agent.Build: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: wag.Name(), Agent: wag, SessionService: sessions, AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	userID := agent.WorkerSessionUser(sessionID)
	for _, err := range r.Run(context.Background(), userID, sessionID, &genai.Content{Role: "user", Parts: []*genai.Part{{Text: text}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
}

// TestNativeContinue_WorkerSeesPriorSessionEvents is the fake-model proof:
// n2, continuing n1 (sessionNodeID = n1's own id, exactly what
// dag.buildGateNodes computes for an ADK-kind resume), lands on n1's own
// deterministic worker session (internal/agent.WorkerSessionID) and its
// request contains n1's prior turn. n3, a fresh sibling, does not.
func TestNativeContinue_WorkerSeesPriorSessionEvents(t *testing.T) {
	bundle, err := agent.LoadBundle("../../agents/web-researcher")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	sessions := session.InMemoryService()
	const chatID = "chat-1"

	n1SessionID := agent.WorkerSessionID(chatID, "n1")
	m1 := &captureModel{}
	runNativeWorkerTurn(t, bundle, sessions, m1, n1SessionID, "PRIOR_ANSWER_MARKER task")

	// n2 continues n1: dag.buildGateNodes sets sessionNodeID to the prior
	// node's own scope for an ADK-kind resume, so this reuses n1's session id.
	m2 := &captureModel{}
	runNativeWorkerTurn(t, bundle, sessions, m2, n1SessionID, "the follow-up task")
	if got := m2.allText(); !strings.Contains(got, "PRIOR_ANSWER_MARKER") {
		t.Fatalf("n2 (continuing n1) request text = %q, want it to contain n1's prior turn", got)
	}

	// n3 is a fresh sibling: its own id, its own session - must not see n1's turn.
	n3SessionID := agent.WorkerSessionID(chatID, "n3")
	m3 := &captureModel{}
	runNativeWorkerTurn(t, bundle, sessions, m3, n3SessionID, "an unrelated task")
	if got := m3.allText(); strings.Contains(got, "PRIOR_ANSWER_MARKER") {
		t.Fatalf("n3 (fresh sibling) request text = %q, must NOT contain n1's prior turn", got)
	}
}
