package rest

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/orchestrator"
)

// injectEvent writes one synthetic event into the orchestrator's chat
// session, replicating what a plan run leaves behind (worker/gate/relay
// events land in the SAME session as the conversation: sessionID == chatID).
func injectEvent(t *testing.T, h *Handler, chatID, author, branch string, content *genai.Content) {
	t.Helper()
	ctx := context.Background()
	resp, err := h.store.Sessions.Get(ctx, &session.GetRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID})
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	ev := session.NewEvent(ctx, "inv-plan-run")
	ev.Author = author
	ev.Branch = branch
	ev.Content = content
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestOrchestratorContextDiet: after a heavy plan run, a follow-up turn's LLM
// request must contain the CONVERSATION — the previous user message, the
// orchestrator's own reply, and the delivered plan answer — and NONE of the
// run's internals. Regression for the live context overflow (2026-07-11): the
// ModeChat orchestrator rebuilt its request from session history, and one
// coding run's worker events (file reads, command output), gate
// prompt-delivery events, and relayed activity inflated the next turn's
// request to ~110K tokens (> the model's 65,536 context). ADK's own filters
// can't exclude them: its branch filter passes everything when the
// requesting invocation's branch is "" (the orchestrator's case) and passes
// every branchless event; foreign-authored events are then CONVERTED into
// user-role "For context: …" text rather than dropped
// (adk/v2 internal/llminternal/contents_processor.go). The fix is the
// conversationSessions view (internal/orchestrator/sessionfilter.go).
func TestOrchestratorContextDiet(t *testing.T) {
	m := &recallModel{}
	h := newTestHandlerWithModel(t, m)
	chatID := "conv-fat"

	postMessage(t, h, chatID, "add flappy bird to my repo")

	// A fat plan-run history: every shape a real run writes into the chat
	// session, each with a distinctive large payload.
	workerPayload := "WORKER-PAYLOAD-A " + strings.Repeat("read_file result line\n", 500)
	gatePrompt := "GATE-PROMPT-PAYLOAD-B " + strings.Repeat("node task prompt text\n", 500)
	relayPayload := "RELAY-PAYLOAD-C " + strings.Repeat("relayed worker output\n", 500)
	// Shape 1: worker event — agent-authored, sub-branched (the local
	// llmagent/RunNode shape; also carries tool traffic in real runs).
	injectEvent(t, h, chatID, "code-implementer", "n1@r1.code-implementer@worker-r0",
		&genai.Content{Role: "model", Parts: []*genai.Part{{Text: workerPayload}}})
	injectEvent(t, h, chatID, "code-implementer", "n1@r1.code-implementer@worker-r0",
		&genai.Content{Role: "model", Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: "fc1", Name: "read_file", Args: map[string]any{"path": "big.go"}},
		}}})
	// Shape 2: gate emitPrompt event — quack-gate author, BRANCHLESS
	// (session.NewEvent stamps no branch), user role.
	injectEvent(t, h, chatID, "quack-gate", "",
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: gatePrompt}}})
	// Shape 3: agent-authored but branchless (the A2A-relay suspicion — either
	// way it must not reach the orchestrator).
	injectEvent(t, h, chatID, "code-implementer", "",
		&genai.Content{Role: "model", Parts: []*genai.Part{{Text: relayPayload}}})
	// The delivered answer, as persistAnswer writes it: orchestrator-authored.
	// "orchestrator" is the author persistAnswer stamps (orchestrator.orchestratorName).
	injectEvent(t, h, chatID, "orchestrator", "",
		&genai.Content{Role: "model", Parts: []*genai.Part{{Text: "DELIVERED-ANSWER: flappy bird added on branch feat/flappy."}}})

	postMessage(t, h, chatID, "thanks — what branch was that on?")

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) < 2 {
		t.Fatalf("model saw %d requests, want 2", len(m.requests))
	}
	second := m.requests[len(m.requests)-1]

	// The conversation survives …
	if !strings.Contains(second, "add flappy bird to my repo") {
		t.Errorf("turn-1 user message missing from the follow-up request")
	}
	if !strings.Contains(second, "REPLY-MARKER-1") {
		t.Errorf("the orchestrator's own turn-1 reply missing from the follow-up request")
	}
	if !strings.Contains(second, "DELIVERED-ANSWER") {
		t.Errorf("the delivered plan answer (orchestrator-authored) missing from the follow-up request")
	}
	// … and the run internals do not.
	for name, payload := range map[string]string{
		"worker event":             "WORKER-PAYLOAD-A",
		"gate prompt event":        "GATE-PROMPT-PAYLOAD-B",
		"branchless relay event":   "RELAY-PAYLOAD-C",
		"foreign-event conversion": "For context:",
	} {
		if strings.Contains(second, payload) {
			t.Errorf("%s leaked into the orchestrator's request (payload %q)", name, payload)
		}
	}
	// Bounded: the request is the small conversation, not the ~34KB of
	// injected run internals.
	if len(second) > 2_000 {
		t.Errorf("follow-up request is %d bytes — the run internals are back in the orchestrator's context", len(second))
	}
}
