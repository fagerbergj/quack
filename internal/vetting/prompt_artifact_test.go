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

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
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

// TestRunGatedRefine_WorkerArtifactsAndPlugins: a native worker
// round's llm.call carries every artifact it resolved (its own system prompt
// plus the agent's memory.md) and the plugin registry rows in scope.
func TestRunGatedRefine_WorkerArtifactsAndPlugins(t *testing.T) {
	stub := &coordsCapturingModel{stubFixedAnswerModel: stubFixedAnswerModel{text: "the answer"}}
	worker, err := llmagent.New(llmagent.Config{
		Name: "tester", Model: stub, Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		JudgeRounds: 0, Agent: "tester", PromptArtifact: "system/web-researcher",
		PromptSource: "static", PromptVersionID: "7",
		MemoryArtifact: artifactsrc.Artifact{Name: "memory/web-researcher", Source: "static", VersionID: "m1"},
		Plugins:        []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}},
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

	want := []ledger.ArtifactRef{
		{Name: "system/web-researcher", Source: "static", VersionID: "7"},
		{Name: "memory/web-researcher", Source: "static", VersionID: "m1"},
	}
	if len(stub.coords.Artifacts) != 2 || stub.coords.Artifacts[0] != want[0] || stub.coords.Artifacts[1] != want[1] {
		t.Errorf("coords.Artifacts = %+v, want %+v", stub.coords.Artifacts, want)
	}
	if len(stub.coords.Plugins) != 1 || stub.coords.Plugins[0].Name != "dotagents" {
		t.Errorf("coords.Plugins = %+v, want [{dotagents abc123}]", stub.coords.Plugins)
	}
}
