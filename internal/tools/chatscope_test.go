package tools

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// markedCtx is a minimal agent.Context whose UserContent carries a given prompt
// text — the ONE channel chatScopeFromContext reads the advisor-thread marker
// from (mirrors how a worker's prompt reaches a co-located or A2A tool call).
type markedCtx struct {
	adkagent.StrictContextMock
	prompt string
}

func (c *markedCtx) UserContent() *genai.Content {
	if c.prompt == "" {
		return nil
	}
	return &genai.Content{Parts: []*genai.Part{{Text: c.prompt}}}
}

func newMarkedCtx(prompt string) *markedCtx {
	return &markedCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()}, prompt: prompt}
}

// TestChatScopeFromContext mirrors guard.go's guardSession derivation: the
// advisor-thread marker in the worker's prompt keys the registry entry the gate
// wrote at node entry, and the per-chat workspace scope is that entry's
// SessionID (== the chat id, since the DAG runs in the chat session).
func TestChatScopeFromContext(t *testing.T) {
	const planID, nodeID = "plan-xyz", "node-1"
	token := vetting.AdvisorThreadToken(planID, nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{SessionID: "chat-abc"})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })

	// A worker prompt with the trailing marker resolves to the chat id.
	prompt := "do the task\n\n" + vetting.AdvisorThreadMarker(token)
	if got := chatScopeFromContext(newMarkedCtx(prompt)); got != "chat-abc" {
		t.Errorf("chatScopeFromContext(marked) = %q, want %q", got, "chat-abc")
	}

	// No marker (a direct/un-gated call) → "" → per-user-root fallback.
	if got := chatScopeFromContext(newMarkedCtx("no marker here")); got != "" {
		t.Errorf("chatScopeFromContext(unmarked) = %q, want \"\" (fallback)", got)
	}

	// A marker whose token isn't registered → "" (no live registration).
	unknown := "task\n\n" + vetting.AdvisorThreadMarker(vetting.AdvisorThreadToken("other", "node"))
	if got := chatScopeFromContext(newMarkedCtx(unknown)); got != "" {
		t.Errorf("chatScopeFromContext(unregistered) = %q, want \"\"", got)
	}

	// A nil context never panics and yields the fallback.
	if got := chatScopeFromContext(nil); got != "" {
		t.Errorf("chatScopeFromContext(nil) = %q, want \"\"", got)
	}
}
