package acp

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// ACP reports no model, so a round's events carry the one the agent's config binds; the dag stream's
// agent_complete, node_done and dag_nodes.model read it from there.
func TestRunPrompt_EventsNameTheBoundModel(t *testing.T) {
	a := usageTestAgent(t, "usage", nil)
	token := vetting.AdvisorThreadToken("plan-1", "impl1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "s1", SessionID: "s1"})
	defer vetting.UnregisterAdvisorThread(token)
	r, err := runner.New(runner.Config{AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := genai.NewContentFromText("add the feature\n\n[[quack:advisor-thread:"+token+"]]", genai.RoleUser)
	lastModel := ""
	for ev, err := range r.Run(vetting.WithAdvisorToken(t.Context(), token), "u1", "s1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		lastModel = ev.ModelVersion
	}
	if lastModel != "qwen3-coder" {
		t.Errorf("the round's final event names model %q, want qwen3-coder", lastModel)
	}
}
