package rest

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// recallModel answers each turn with a marker and records the full text of
// every request it receives, so a test can assert what history the
// orchestrator's llmagent actually saw on a follow-up turn.
type recallModel struct {
	mu       sync.Mutex
	requests []string // one concatenated text blob per GenerateContent call
}

func (*recallModel) Name() string { return "recall" }

func (m *recallModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var sb strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteString("\n")
			}
		}
	}
	m.mu.Lock()
	m.requests = append(m.requests, sb.String())
	n := len(m.requests)
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "REPLY-MARKER-" + string(rune('0'+n))}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func postMessage(t *testing.T, h *Handler, chatID, content string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/responses",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.SendChatMessage(rec, req, chatID)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %q: status %d", content, rec.Code)
	}
}

// TestOrchestratorRemembersConversation: the SECOND turn's LLM request must
// contain the first turn's user message AND the orchestrator's own first reply.
// Regression for the amnesia bug (live e2e 2026-07-05): the orchestrator
// llmagent runs wrapped in a workflow AgentNode, which forces an UNSET mode to
// ModeSingleTurn — discarding all session history — so one turn after
// delivering a plan it answered "I don't see a previously created plan in our
// conversation". llmagent.Config now pins Mode: ModeChat.
func TestOrchestratorRemembersConversation(t *testing.T) {
	m := &recallModel{}
	h := newTestHandlerWithModel(t, m)
	chatID := "conv-1"

	postMessage(t, h, chatID, "remember the code word quackers")
	postMessage(t, h, chatID, "what is the code word?")

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) < 2 {
		t.Fatalf("model saw %d requests, want 2", len(m.requests))
	}
	second := m.requests[len(m.requests)-1]
	if !strings.Contains(second, "quackers") {
		t.Errorf("turn 2 request lost turn 1's user message:\n%s", second)
	}
	if !strings.Contains(second, "REPLY-MARKER-1") {
		t.Errorf("turn 2 request lost the orchestrator's own turn-1 reply:\n%s", second)
	}
}
