package acp

import (
	"context"
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
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	cwd, _, err := a.resolveNode(context.Background(), "review the PR\n\n"+vetting.AdvisorThreadMarker(token))
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

// TestResolveNodeWorktreeParentWithoutHookErrors: a node that needs a
// worktree but has no Worktree executor configured is a wiring bug - fail
// loudly, mirroring dag.SetupFunc's nil-executor error, rather than silently
// falling back to a bare directory that would hand the node an empty dir
// with none of the parent clone's content.
func TestResolveNodeWorktreeParentWithoutHookErrors(t *testing.T) {
	a := &Agent{opts: Options{UserID: "u1"}}
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "review1", WorkspaceNodeID: "review1", WorktreeParent: workspace.SharedRepoScope, SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	_, _, err := a.resolveNode(context.Background(), "review\n\n"+vetting.AdvisorThreadMarker(token))
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
		NodeID: "impl1", WorkspaceNodeID: "impl1", SessionID: "chat1",
	})
	defer vetting.UnregisterAdvisorThread(token)

	cwd, _, err := a.resolveNode(context.Background(), "implement\n\n"+vetting.AdvisorThreadMarker(token))
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
