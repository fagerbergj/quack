package serve

import (
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack-extensions/github"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Agents STAGE (tool calls, git commits); the gate DELIVERS - #669 (see
// config/quack.yaml's guards comment). This file asserts that invariant the
// way the runtime actually resolves tools (buildAgents/opencodeEnv), not from
// a hand-written list of tool names - a name added on either side (a new
// ExtTool, a new agent) is caught automatically, only a genuinely new WRITE
// capability needs a line here.

func requireStageDeliverEnv(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"QUACK_LLM_ENDPOINT", "http://x/v1"}, {"QUACK_LLM_API_KEY", "k"}, {"QUACK_DATABASE_URL", "postgres://localhost/db"},
		{"QUACK_ORCH_MODEL", "m"}, {"QUACK_RESEARCHER_MODEL", "r"}, {"QUACK_MEDIA_MODEL", "md"}, {"QUACK_IMAGE_MODEL", "im"},
		{"QUACK_JUDGE_MODEL", "j"}, {"QUACK_EMBED_MODEL", "e"}, {"QUACK_SEARXNG_URL", "http://s"}, {"QUACK_CRAWL4AI_URL", "http://c"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

// mutatingGitHubTools names every tool that writes to shared GitHub state.
// Derived from the SAME extension buildAgents wires into ExtTools
// (github.App.Tools()), not hand-listed: App.Tools()'s own doc comment
// commits it to outbound-posting tools ONLY ("NOT here: anything that opens
// a PR or submits a review" - delivery stays gate-owned), so every name it
// returns today is a write by that contract, and a future addition there is
// picked up here with no edit.
func mutatingGitHubTools() map[string]bool {
	names := map[string]bool{}
	for _, tl := range (&github.App{}).Tools() {
		names[tl.Name()] = true
	}
	return names
}

// nativeAgentGitHubWriteGrants resolves every NATIVE agent's real tool set
// exactly as buildAgents does - resolveToolNames, then tools.Build against
// the live registry plus the github extension's tools - and reports
// "<agent>: <tool>" for each resolved tool matching mutating. ACP agents are
// skipped: config's tools: is ignored for them (they carry no quack tools at
// all - internal/config's AcpAgentConfig doc comment).
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
		toolNames, _ := resolveToolNames(ac.Tools, true, false)
		if len(toolNames) == 0 {
			continue
		}
		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			t.Fatalf("agent %q: unknown provider %q", name, ac.Provider)
		}
		wm, err := inference.NewModel(prov, ac.Model, nil)
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

// TestNoNativeAgentGrantedGitHubWriteTool is the #669 drift test's first
// half: no agent in config/quack.yaml resolves, through the real build path,
// to a tool that can push/comment/review/create-issue on GitHub. Currently
// green because no agent's tools: list names github_comment,
// github_reply_to_review_comment or github_react_to_comment - the only
// extension tools the github App exposes.
func TestNoNativeAgentGrantedGitHubWriteTool(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := nativeAgentGitHubWriteGrants(t, cfg, mutatingGitHubTools()); len(got) != 0 {
		t.Fatalf("agent(s) resolve to a GitHub-mutating tool - stage-then-deliver is broken: %v", got)
	}
}

// TestGitHubWriteGrantCheckCatchesHypotheticalGrant proves
// nativeAgentGitHubWriteGrants is not vacuous: granting a hypothetical
// mutating tool (issue #669's own example) to any agent must fail it. Uses a
// synthetic config rather than editing the shipped one.
func TestGitHubWriteGrantCheckCatchesHypotheticalGrant(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.Load("../../config/quack.yaml")
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

// TestGitPushToolNotBuildable pins fact #2 from #669: the native write-side
// tools (including a "git_push" agent tool) were deleted in 0.6.0 when code
// agents moved to ACP. There is nothing named git_push in the builtin
// registry NOR the github extension for an agent to be granted, by
// construction - tools.Build must refuse to resolve it under any config.
func TestGitPushToolNotBuildable(t *testing.T) {
	if _, err := tools.Build([]string{"git_push"}, tools.Deps{}); err == nil {
		t.Fatal("tools.Build resolved \"git_push\" - a write-side tool has reappeared in the agent-callable registry; see #669")
	}
}

// acpPermissionDeniesGitPush parses an ACP agent's generated
// OPENCODE_CONFIG_CONTENT env entry and reports whether its bash permission
// policy denies both "git push" and "git push *".
func acpPermissionDeniesGitPush(t *testing.T, env []string) bool {
	t.Helper()
	var raw string
	const prefix = "OPENCODE_CONFIG_CONTENT="
	for _, kv := range env {
		if len(kv) > len(prefix) && kv[:len(prefix)] == prefix {
			raw = kv[len(prefix):]
		}
	}
	if raw == "" {
		t.Fatal("opencodeEnv produced no OPENCODE_CONFIG_CONTENT entry")
	}
	var cfg struct {
		Permission struct {
			Bash map[string]string `json:"bash"`
		} `json:"permission"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse OPENCODE_CONFIG_CONTENT: %v", err)
	}
	return cfg.Permission.Bash["git push"] == "deny" && cfg.Permission.Bash["git push *"] == "deny"
}

// TestACPAgentPermissionDeniesGitPush is the #669 drift test's second half:
// every ACP agent's generated opencode permission config denies git push -
// checked against opencodeEnv, the SAME function buildAgents calls to build
// the subprocess env, not a hand-written policy string.
func TestACPAgentPermissionDeniesGitPush(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	found := false
	for name, ac := range cfg.Agents {
		if ac.Acp == nil {
			continue
		}
		found = true
		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			t.Fatalf("agent %q: unknown provider %q", name, ac.Provider)
		}
		for _, mode := range []workspace.SandboxMode{workspace.SandboxLandlock, workspace.SandboxBwrap, workspace.SandboxNone} {
			env := opencodeEnv(prov, ac, nil, mode)
			if !acpPermissionDeniesGitPush(t, env) {
				t.Errorf("agent %q (sandbox %s): generated opencode permission config does not deny git push - see #669", name, mode)
			}
		}
	}
	if !found {
		t.Fatal("no ACP agent found in config/quack.yaml - the ACP half of this test asserted nothing")
	}
}

// TestACPGitPushDenyCheckCatchesFlippedPermission proves
// acpPermissionDeniesGitPush is not vacuous: flipping the policy to allow
// git push (issue #669's own test case 2) must fail it.
func TestACPGitPushDenyCheckCatchesFlippedPermission(t *testing.T) {
	allowed := []string{`OPENCODE_CONFIG_CONTENT={"permission":{"bash":{"git push":"allow","git push *":"allow","*":"allow"}}}`}
	if acpPermissionDeniesGitPush(t, allowed) {
		t.Fatal("a config allowing git push passed the deny check - it would silently pass a real hole")
	}
}
