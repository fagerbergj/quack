package vetting

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	workflowagent "google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// TestRunGatedRefine_WorkerPromptArtifactFallsBackToCfgNotAgentKey guards the
// H2 gap: with no live RefreshPrompt (or one that returns no VersionID), the
// stamped langfuse.observation.prompt.name must be Config.PromptArtifact (the
// bundle-resolved name, e.g. "system/web-researcher") - NOT "system/"+Agent,
// which is wrong whenever the agent's config key differs from its bundle dir
// (repro: agent key "tester", bundle agents/web-researcher).
func TestRunGatedRefine_WorkerPromptArtifactFallsBackToCfgNotAgentKey(t *testing.T) {
	stub := &coordsCapturingModel{stubFixedAnswerModel: stubFixedAnswerModel{text: "the answer"}}
	worker, err := llmagent.New(llmagent.Config{
		Name: "tester", Model: stub, Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		JudgeRounds: 0, Agent: "tester", PromptArtifact: "system/web-researcher",
		PromptSource: "prod-langfuse", PromptVersionID: "7",
	}
	node, err := newTestGatedNode("researcher-gate", worker, stub, nil, cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "question"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if stub.coords.PromptArtifact != "system/web-researcher" {
		t.Errorf("coords.PromptArtifact = %q, want system/web-researcher (cfg.PromptArtifact), not system/%s", stub.coords.PromptArtifact, cfg.Agent)
	}
}
