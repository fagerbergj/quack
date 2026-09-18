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
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestConfigHasWorkspaceClone pins the predicate serve.buildGateJudge uses to
// pick a node's judge tool set (#1485): an ACP node with a jail configured has
// a clone; a native node, or a deployment with no jail at all, never does -
// regardless of agent name.
func TestConfigHasWorkspaceClone(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("jail: %v", err)
	}
	for _, tc := range []struct {
		name           string
		externalWorker bool
		jail           *workspace.Jail
		want           bool
	}{
		{"ACP node with jail", true, jail, true},
		{"native node with jail (web-researcher/synthesizer)", false, jail, false},
		{"ACP node with no jail configured", true, nil, false},
		{"native node with no jail configured", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ExternalWorker: tc.externalWorker, Workspace: tc.jail}
			if got := cfg.HasWorkspaceClone(); got != tc.want {
				t.Errorf("HasWorkspaceClone() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunGatedRefine_RefreshJudgeBindingHasReadToolsFollowsNode proves prepareJudge
// passes the ROUND'S OWN node cfg.HasWorkspaceClone() into RefreshJudgeBinding, not
// a fixed deployment-wide value - a code node keeps read-tool eligibility, a
// research node (no clone) never gets it, even though gates.judge builds one
// judge model shared by every node (#1485).
func TestRunGatedRefine_RefreshJudgeBindingHasReadToolsFollowsNode(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("jail: %v", err)
	}
	for _, tc := range []struct {
		name           string
		externalWorker bool
		want           bool
	}{
		{"code node (ACP, has a clone)", true, true},
		{"research node (native, no clone)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &judgeModelCoordsSpy{}
			worker, err := llmagent.New(llmagent.Config{
				Name: "worker", Model: stubFixedAnswerModel{text: "the answer"},
				Description: "worker", Instruction: "Answer.",
			})
			if err != nil {
				t.Fatalf("worker: %v", err)
			}
			var gotHasReadTools bool
			factory := NewJudgeFactory(spy, nil, nil)
			cfg := Config{
				JudgeRounds: 1, Threshold: 0.5, Rubric: "score 0-10",
				ChatID: "chat1", Agent: "worker", Source: "github", JudgeModel: spy,
				ExternalWorker: tc.externalWorker, Workspace: jail,
				RefreshJudgeBinding: func(art artifactsrc.Artifact, hasReadTools bool) (JudgeFactory, model.LLM, string) {
					gotHasReadTools = hasReadTools
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
			if gotHasReadTools != tc.want {
				t.Errorf("RefreshJudgeBinding hasReadTools = %v, want %v", gotHasReadTools, tc.want)
			}
		})
	}
}

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
		RefreshJudgeBinding: func(art artifactsrc.Artifact, hasReadTools bool) (JudgeFactory, model.LLM, string) {
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
