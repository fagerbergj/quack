package acp

import (
	"context"
	"os"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestResolveNodeWorktreeParentInvokesWorktreeHook: a node with WorktreeParent resolves its cwd via
// Options.Worktree, not Jail.EnsureDir, with parent/node workspace ids passed through.
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

	cwd, _, _, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
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

// TestResolveNodeUsesChatIDNotSessionIDOnRetry: RetryNode runs under a synthetic "chatID::retry" session id,
// so resolveNode must key workspace calls off ChatID or resolve into an unprovisioned scope.
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

	if _, _, _, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token)); err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if gotChat != "chat1" {
		t.Errorf("Worktree called with chatID=%q, want the real chat scope %q (not the retry session id)", gotChat, "chat1")
	}
}

// TestResolveNodeWorktreeParentWithoutHookErrors: needing a worktree with no Worktree executor is a wiring bug;
// fail loudly rather than hand the node an empty dir.
func TestResolveNodeWorktreeParentWithoutHookErrors(t *testing.T) {
	a := &Agent{opts: Options{UserID: "u1"}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, ChatID: "chat1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, _, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
	if err == nil {
		t.Fatal("resolveNode: want an error - the node needs a worktree but none is configured")
	}
}

// TestResolveNodeNonWorktreeNodeUsesJail: a node with no WorktreeParent resolves via Jail.EnsureDir.
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

	cwd, _, _, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
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

// TestResolveNodeReturnsAdvisorTaskReadOnly: resolveNode surfaces this node's AdvisorTask.ReadOnly
// (set per run, not static config) so runPrompt can build per-round Caps.
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
		_, _, _, _, got, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
		vetting.UnregisterAdvisorThread(token)
		if err != nil {
			t.Fatalf("resolveNode: %v", err)
		}
		if got != want {
			t.Errorf("resolveNode readOnly = %v, want %v", got, want)
		}
	}
}

// TestResolveNodeGrantsScratchDir: resolveNode creates a per-node scratch dir under the cwd's coordinates,
// distinct from the cwd, whatever the node's ReadOnly flag.
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
		cwd, _, scratchDir, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
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

// TestResolveNodeNoJailNoScratchDir: no Jail configured degrades scratchDir
// to "", never a panic.
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

	_, _, scratchDir, _, _, _, _, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if scratchDir != "" {
		t.Errorf("scratchDir = %q, want \"\" with no Jail configured", scratchDir)
	}
}

// TestResolveNodeChatAndNodeIDKeyOffAdvisorThread: a setup-chain writer's WorkspaceNodeID collapses to the repo
// scope, so resolveNode returns the advisor thread's ChatID/NodeID or live steer registers under a dead key.
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

	_, _, _, _, _, chatID, nodeID, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), token))
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if chatID != "chat1" || nodeID != "impl2" {
		t.Errorf("resolveNode chat/node = (%q,%q), want (chat1,impl2) - not the shared workspace scope %q or the retry session id", chatID, nodeID, workspace.SharedRepoScope)
	}
}
