package acp

import (
	"context"
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestResolveNodeWorktreeParentInvokesWorktreeHook pins the acp side of Part
// A: a node whose AdvisorTask carries a WorktreeParent (a read-only
// qualifying node - reviewer, explorer - in a plan.Setup chain) must have its
// cwd resolved through Options.Worktree, not the plain Jail.EnsureDir path,
// with the parent/this-node workspace ids threaded through unchanged.
func TestResolveNodeWorktreeParentInvokesWorktreeHook(t *testing.T) {
	var gotUser, gotChat, gotParent, gotNode string
	a := &Agent{opts: Options{
		UserID: "u1",
		Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
			gotUser, gotChat, gotParent, gotNode = userID, chatID, parentNodeID, nodeID
			return "/resolved/worktree/dir", nil
		},
	}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	cwd, _, _, _, _, _, _, err := a.resolveNode(context.Background(), "review the PR\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if cwd != "/resolved/worktree/dir" {
		t.Errorf("cwd = %q, want the Worktree hook's return value", cwd)
	}
	if gotUser != "u1" || gotChat != "chat1" || gotParent != workspace.SharedRepoScope || gotNode != "review1" {
		t.Errorf("Worktree called with (%q,%q,%q,%q), want (u1,chat1,%q,review1)", gotUser, gotChat, gotParent, gotNode, workspace.SharedRepoScope)
	}
}

// TestResolveNodeUsesChatIDNotSessionIDOnRetry pins #997: RetryNode (both
// REST retry and boot-resume ride it) runs the re-entered node under a
// synthetic ADK session id ("chatID::retry", see orchestrator.RetryNode) -
// distinct from the real chat scope the setup clone was provisioned under.
// resolveNode must key every workspace call off ChatID, never SessionID,
// or a retried worktree/reviewer/explorer node resolves into a scope the
// clone was never provisioned in (the issue's "worktree add ... no such
// file or directory").
func TestResolveNodeUsesChatIDNotSessionIDOnRetry(t *testing.T) {
	var gotChat string
	a := &Agent{opts: Options{
		UserID: "u1",
		Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
			gotChat = chatID
			return "/resolved/worktree/dir", nil
		},
	}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope,
		ChatID: "chat1", SessionID: "chat1::retry",
	})
	defer vetting.UnregisterAdvisorThread(token)

	if _, _, _, _, _, _, _, err := a.resolveNode(context.Background(), "review the PR\n\n"+vetting.AdvisorThreadMarker(token)); err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if gotChat != "chat1" {
		t.Errorf("Worktree called with chatID=%q, want the real chat scope %q (not the retry session id)", gotChat, "chat1")
	}
}

// TestResolveNodeWorktreeParentWithoutHookErrors: a node that needs a
// worktree but has no Worktree executor configured is a wiring bug - fail
// loudly, mirroring dag.SetupFunc's nil-executor error, rather than silently
// falling back to a bare directory that would hand the node an empty dir
// with none of the parent clone's content.
func TestResolveNodeWorktreeParentWithoutHookErrors(t *testing.T) {
	a := &Agent{opts: Options{UserID: "u1"}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, _, _, _, _, _, err := a.resolveNode(context.Background(), "review\n\n"+vetting.AdvisorThreadMarker(token))
	if err == nil {
		t.Fatal("resolveNode: want an error - the node needs a worktree but none is configured")
	}
}

// TestResolveNodeNonWorktreeNodeUsesJail: a node with no WorktreeParent (a
// writer, or a plain non-Setup node) still resolves via the plain
// Jail.EnsureDir path, unaffected by worktree isolation.
func TestResolveNodeNonWorktreeNodeUsesJail(t *testing.T) {
	dir := t.TempDir()
	jail, err := workspace.NewJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{UserID: "u1", Jail: jail}}
	token := vetting.AdvisorThreadToken("plan-1", "impl1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	cwd, _, _, _, _, _, _, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	want, err := jail.Resolve("u1", "chat1", workspace.NodeDir("impl1"))
	if err != nil {
		t.Fatal(err)
	}
	if cwd != want {
		t.Errorf("cwd = %q, want the jail-resolved node dir %q", cwd, want)
	}
}

// TestResolveNodeReturnsAdvisorTaskReadOnly is #754's acp-side pin:
// resolveNode surfaces THIS node's AdvisorTask.ReadOnly (set per-run by
// dag.nodeGateConfig, not the agent's static config) so runPrompt can build
// per-round Caps with it - the seam a planOnly run's dynamic override needs.
func TestResolveNodeReturnsAdvisorTaskReadOnly(t *testing.T) {
	dir := t.TempDir()
	jail, err := workspace.NewJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{UserID: "u1", Jail: jail}}
	for _, want := range []bool{true, false} {
		token := vetting.AdvisorThreadToken("plan-1", "impl1")
		vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
			NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "chat1", SessionID: "chat1", ReadOnly: want,
		})
		_, _, _, _, got, _, _, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
		vetting.UnregisterAdvisorThread(token)
		if err != nil {
			t.Fatalf("resolveNode: %v", err)
		}
		if got != want {
			t.Errorf("resolveNode readOnly = %v, want %v", got, want)
		}
	}
}

// TestResolveNodeGrantsContextDir pins the #659/#660 wiring: resolveNode
// derives the GitHub trigger's sibling context directory from the SAME
// (UserID, SessionID) coordinate the dispatch side writes it under - no
// separate registry field needed - so the sandbox actually grants it.
func TestResolveNodeGrantsContextDir(t *testing.T) {
	dir := t.TempDir()
	jail, err := workspace.NewJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{UserID: "u1", Jail: jail}}
	token := vetting.AdvisorThreadToken("plan-1", "impl1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, ctxDir, _, _, _, _, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	want, err := jail.Resolve("u1", "chat1", workspace.ContextDirScope)
	if err != nil {
		t.Fatal(err)
	}
	if ctxDir != want {
		t.Errorf("ctxDir = %q, want the jail-resolved context dir %q", ctxDir, want)
	}
}

// TestResolveNodeNoJailNoContextDir: a test harness Agent with no Jail
// configured (see the WorktreeParent tests above) must not panic resolving
// ctxDir - it degrades to "" (no extra grant), never a nil-pointer crash.
func TestResolveNodeNoJailNoContextDir(t *testing.T) {
	a := &Agent{opts: Options{
		UserID: "u1",
		Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
			return "/resolved/worktree/dir", nil
		},
	}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, ctxDir, _, _, _, _, err := a.resolveNode(context.Background(), "review\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if ctxDir != "" {
		t.Errorf("ctxDir = %q, want \"\" with no Jail configured", ctxDir)
	}
}

// TestResolveNodeGrantsScratchDir pins the writable-scratch fix: resolveNode
// derives a per-node scratch dir (workspace.Jail.ScratchDir) from the SAME
// (UserID, SessionID, WorkspaceNodeID) coordinate the cwd resolves under -
// created, and distinct from the node's own working directory - regardless
// of the node's ReadOnly flag (a read-only reviewer needs scratch exactly as
// much as a writer does).
func TestResolveNodeGrantsScratchDir(t *testing.T) {
	dir := t.TempDir()
	jail, err := workspace.NewJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{UserID: "u1", Jail: jail}}
	for _, readOnly := range []bool{true, false} {
		token := vetting.AdvisorThreadToken("plan-1", "impl1")
		vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
			NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "chat1", SessionID: "chat1", ReadOnly: readOnly,
		})
		cwd, _, _, scratchDir, _, _, _, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
		vetting.UnregisterAdvisorThread(token)
		if err != nil {
			t.Fatalf("resolveNode (readOnly=%v): %v", readOnly, err)
		}
		want, err := jail.ScratchDir("u1", "chat1", "impl1")
		if err != nil {
			t.Fatal(err)
		}
		if scratchDir != want {
			t.Errorf("scratchDir (readOnly=%v) = %q, want %q", readOnly, scratchDir, want)
		}
		if scratchDir == cwd {
			t.Errorf("scratchDir must not be the node's own working directory: both are %q", scratchDir)
		}
		if info, statErr := os.Stat(scratchDir); statErr != nil || !info.IsDir() {
			t.Errorf("scratchDir %q was not created: %v", scratchDir, statErr)
		}
	}
}

// TestResolveNodeNoJailNoScratchDir mirrors TestResolveNodeNoJailNoContextDir
// for the scratch grant: no Jail configured degrades to "", never a panic.
func TestResolveNodeNoJailNoScratchDir(t *testing.T) {
	a := &Agent{opts: Options{
		UserID: "u1",
		Worktree: func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (string, error) {
			return "/resolved/worktree/dir", nil
		},
	}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, _, scratchDir, _, _, _, err := a.resolveNode(context.Background(), "review\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if scratchDir != "" {
		t.Errorf("scratchDir = %q, want \"\" with no Jail configured", scratchDir)
	}
}

// TestResolveNodeChatAndNodeIDKeyOffAdvisorThread (#998 review): a setup
// chain's writer node has WorkspaceNodeID collapsed to the shared repo scope
// (dag.workspaceNodeID) while NodeID stays the plan node's own id - the
// executor's controls (and REST QueueNodeMessage) are keyed by the LATTER, so
// resolveNode must return the advisor thread's own ChatID/NodeID (the real
// chat scope, not the ADK SessionID a retry aliases - see AdvisorTask.ChatID)
// and never the workspace scope, or the live-steer hook registers under a
// key nothing looks up.
func TestResolveNodeChatAndNodeIDKeyOffAdvisorThread(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{UserID: "u1", Jail: jail}}
	token := vetting.AdvisorThreadToken("plan-1", "impl2")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "impl2", WorkspaceNodeID: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1::retry",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, _, _, _, chatID, nodeID, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if chatID != "chat1" || nodeID != "impl2" {
		t.Errorf("resolveNode chat/node = (%q,%q), want (chat1,impl2) - not the shared workspace scope %q or the retry session id", chatID, nodeID, workspace.SharedRepoScope)
	}
}
