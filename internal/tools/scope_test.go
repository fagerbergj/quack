package tools

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// markedCtx is a minimal agent.Context whose UserContent carries a given prompt
// text - the ONE channel chatScopeFromContext reads the advisor-thread marker
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

// TestScopeFromContext mirrors guard.go's guardSession derivation: the
// advisor-thread marker in the worker's prompt keys the registry entry the gate
// wrote at node entry - the per-chat workspace scope is that entry's ChatID
// (distinct from SessionID, the ADK session id, which a retry re-derives) and
// the node's own working dir is its NodeID.
func TestScopeFromContext(t *testing.T) {
	const planID, nodeID = "plan-xyz", "node-1"
	token := vetting.AdvisorThreadToken(planID, nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: "chat-abc", SessionID: "chat-abc::retry", NodeID: nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })

	// A worker prompt with the trailing marker resolves to the chat id + node dir.
	prompt := "do the task\n\n" + vetting.AdvisorThreadMarker(token)
	if chatID, nodeDir := scopeFromContext(newMarkedCtx(prompt)); chatID != "chat-abc" || nodeDir != nodeID {
		t.Errorf("scopeFromContext(marked) = (%q, %q), want (chat-abc, %s)", chatID, nodeDir, nodeID)
	}

	// No marker (a direct/un-gated call) → no scope → per-user-root fallback.
	if chatID, nodeDir := scopeFromContext(newMarkedCtx("no marker here")); chatID != "" || nodeDir != "" {
		t.Errorf("scopeFromContext(unmarked) = (%q, %q), want empty (fallback)", chatID, nodeDir)
	}

	// A marker whose token isn't registered → no scope (no live registration).
	unknown := "task\n\n" + vetting.AdvisorThreadMarker(vetting.AdvisorThreadToken("other", "node"))
	if chatID, nodeDir := scopeFromContext(newMarkedCtx(unknown)); chatID != "" || nodeDir != "" {
		t.Errorf("scopeFromContext(unregistered) = (%q, %q), want empty", chatID, nodeDir)
	}

	// A nil context never panics and yields the fallback.
	if chatID, nodeDir := scopeFromContext(nil); chatID != "" || nodeDir != "" {
		t.Errorf("scopeFromContext(nil) = (%q, %q), want empty", chatID, nodeDir)
	}
}
