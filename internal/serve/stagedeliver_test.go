package serve

import (
	"testing"

	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack-extensions/github"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Agents stage (tool calls, git commits); the gate delivers. These tests check that through buildAgents'
// real tool resolution, so only a genuinely new write capability needs a line here.

func requireStageDeliverEnv(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "qwen3.8-27b"}, {"QUACK_RESEARCHER_MODEL", "qwen3.8-27b"}, {"QUACK_MEDIA_MODEL", "qwen3-omni-30b"}, {"QUACK_IMAGE_MODEL", "qwen3-vl-32b"},
		{"QUACK_JUDGE_MODEL", "gemma4-26b-a4b"}, {"QUACK_EMBED_MODEL", "qwen3-embed"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

// mutatingGitHubTools names every tool that writes to shared GitHub state, taken from github.App.Tools(),
// whose contract is outbound-posting tools only, so new ones are picked up without an edit.
func mutatingGitHubTools() map[string]bool {
	names := map[string]bool{}
	for _, tl := range (&github.App{}).Tools() {
		names[tl.Name()] = true
	}
	return names
}

// nativeAgentGitHubWriteGrants resolves each native agent's tools as buildAgents does and reports
// "<agent>: <tool>" for each mutating one. ACP agents carry no quack tools and are skipped.
func nativeAgentGitHubWriteGrants(t *testing.T, cfg *config.Config, mutating map[string]bool) []string {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}
	extToolsByName := map[string]tool.Tool{}
	for _, tl := range (&github.App{}).Tools() {
		extToolsByName[tl.Name()] = tl
	}

	var violations []string
	for name, ac := range cfg.Agents {
		if ac.Acp != nil {
			continue
		}
		toolNames := resolveToolNames(ac.Tools, true)
		if len(toolNames) == 0 {
			continue
		}
		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			t.Fatalf("agent %q: unknown provider %q", name, ac.Provider)
		}
		wm, err := inference.NewModel(prov, ac.Model, nil, cfg.ModelCost(ac.Model), "")
		if err != nil {
			t.Fatalf("agent %q: model: %v", name, err)
		}
		built, err := tools.Build(toolNames, tools.Deps{
			WebSearch:       tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
			Fetch:           tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
			Summarizer:      wm,
			Workspace:       jail,
			WorkspaceUserID: "local",
			ExtTools:        extToolsByName,
		})
		if err != nil {
			if ac.Optional {
				// Mirrors buildAgents' own degrade-honestly path (serve.go): an
				// optional agent whose extension is off in this config just drops out.
				continue
			}
			t.Fatalf("agent %q: resolve tools the way the runtime would: %v", name, err)
		}
		for _, tl := range built {
			if mutating[tl.Name()] {
				violations = append(violations, name+": "+tl.Name())
			}
		}
	}
	return violations
}

// TestNoNativeAgentGrantedGitHubWriteTool: no agent in config/quack.yaml resolves, through the real build
// path, to a tool that can push, comment, review or create issues on GitHub.
func TestNoNativeAgentGrantedGitHubWriteTool(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := nativeAgentGitHubWriteGrants(t, cfg, mutatingGitHubTools()); len(got) != 0 {
		t.Fatalf("agent(s) resolve to a GitHub-mutating tool - stage-then-deliver is broken: %v", got)
	}
}

// TestGitHubWriteGrantCheckCatchesHypotheticalGrant: granting a hypothetical mutating tool to any agent
// fails the check, so it isn't vacuous.
func TestGitHubWriteGrantCheckCatchesHypotheticalGrant(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rogue := cfg.Agents["web-researcher"]
	rogue.Tools = append(append([]string{}, rogue.Tools...), "github_comment")
	cfg.Agents["web-researcher"] = rogue

	got := nativeAgentGitHubWriteGrants(t, cfg, mutatingGitHubTools())
	if len(got) == 0 {
		t.Fatal("granting github_comment to an agent did not trip the check - it would silently pass a real hole")
	}
}

// TestGitPushToolNotBuildable: nothing named git_push exists in the builtin registry or the github
// extension, so tools.Build refuses it under any config.
func TestGitPushToolNotBuildable(t *testing.T) {
	if _, err := tools.Build([]string{"git_push"}, tools.Deps{}); err == nil {
		t.Fatal("tools.Build resolved \"git_push\" - a write-side tool has reappeared in the agent-callable registry; see #669")
	}
}
