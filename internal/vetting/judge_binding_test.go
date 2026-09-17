package vetting

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	workflowagent "google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
)

// TestRunGatedRefine_RefreshesJudgeBindingEachRound proves prepareJudge calls
// Config.RefreshJudgeBinding once per judge round with system/judge's resolved
// artifact, and applies its returned thinking_level to that round (#1421 P2).
func TestRunGatedRefine_RefreshesJudgeBindingEachRound(t *testing.T) {
	spy := &judgeModelCoordsSpy{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stubFixedAnswerModel{text: "the answer"},
		Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}

	var calls int
	var lastArtName string
	factory := NewJudgeFactory(spy, nil, nil)
	cfg := Config{
		JudgeRounds: 1, Threshold: 0.5, Rubric: "score 0-10",
		ChatID: "chat1", Agent: "web-researcher", Source: "github", JudgeModel: spy,
		RefreshJudgeBinding: func(art artifactsrc.Artifact) (JudgeFactory, model.LLM, string) {
			calls++
			lastArtName = art.Name
			return factory, spy, "high"
		},
	}
	node, err := newTestGatedNode("gate", worker, stubFixedAnswerModel{}, factory, cfg)
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

	if calls != 1 {
		t.Fatalf("RefreshJudgeBinding calls = %d, want 1 (one judge round)", calls)
	}
	if lastArtName != "system/judge" {
		t.Errorf("resolved artifact name = %q, want system/judge", lastArtName)
	}
}

// TestRunGatedRefine_JudgeArtifactsAndPlugins: the judge round's
// llm.call carries system/judge plus whichever rubric/constitution artifacts
// this node resolved, and the plugin registry rows in scope for the round.
func TestRunGatedRefine_JudgeArtifactsAndPlugins(t *testing.T) {
	spy := &judgeModelCoordsSpy{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stubFixedAnswerModel{text: "the answer"},
		Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		JudgeRounds: 1, Threshold: 0.5, Rubric: "score 0-10",
		ChatID: "chat1", Agent: "web-researcher", Source: "github", JudgeModel: spy,
		RubricArtifact:       artifactsrc.Artifact{Name: "rubric/global", Source: "static", VersionID: "r1"},
		ConstitutionArtifact: artifactsrc.Artifact{Name: "rubric/constitution", Source: "static", VersionID: "c1"},
		Plugins:              []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}},
	}
	node, err := newTestGatedNode("gate", worker, stubFixedAnswerModel{}, NewJudgeFactory(spy, nil, nil), cfg)
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

	byName := map[string]ledger.ArtifactRef{}
	for _, a := range spy.stamped.Artifacts {
		byName[a.Name] = a
	}
	if _, ok := byName["system/judge"]; !ok {
		t.Errorf("stamped artifacts = %+v, want system/judge present", spy.stamped.Artifacts)
	}
	if got, want := byName["rubric/global"], (ledger.ArtifactRef{Name: "rubric/global", Source: "static", VersionID: "r1"}); got != want {
		t.Errorf("rubric/global artifact = %+v, want %+v", got, want)
	}
	if got, want := byName["rubric/constitution"], (ledger.ArtifactRef{Name: "rubric/constitution", Source: "static", VersionID: "c1"}); got != want {
		t.Errorf("rubric/constitution artifact = %+v, want %+v", got, want)
	}
	if len(spy.stamped.Plugins) != 1 || spy.stamped.Plugins[0].Name != "dotagents" || spy.stamped.Plugins[0].SHA != "abc123" {
		t.Errorf("stamped plugins = %+v, want [{dotagents abc123}]", spy.stamped.Plugins)
	}
}
