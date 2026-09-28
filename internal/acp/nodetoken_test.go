package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// registerSeededNode registers a live node thread and seeds <nodeID>.txt in its workspace dir.
func registerSeededNode(t *testing.T, a *Agent, planID, nodeID string, at vetting.AdvisorTask) string {
	t.Helper()
	token := vetting.AdvisorThreadToken(planID, nodeID)
	at.NodeID, at.WorkspaceNodeID = nodeID, nodeID
	vetting.RegisterAdvisorThread(token, at)
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	dir, err := a.opts.Jail.EnsureDir("u1", at.ChatID, workspace.NodeDir(nodeID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, nodeID+".txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return token
}

// TestResolveNode_CtxTokenOnly: the round's scope is the ctx token's registration; a
// missing or unregistered token fails before any cwd, secret or spawn.
func TestResolveNode_CtxTokenOnly(t *testing.T) {
	a := testAgent(t, "echo")
	own := registerSeededNode(t, a, "p", "own", vetting.AdvisorTask{ChatID: "c1", ReadOnly: true, MemSecret: "sec-own"})
	registerSeededNode(t, a, "p-evil", "evil", vetting.AdvisorTask{ChatID: "c-evil", MemSecret: "sec-evil"})

	cwd, secret, _, _, ro, _, nodeID, _, _, err := a.resolveNode(vetting.WithAdvisorToken(context.Background(), own))
	if err != nil || secret != "sec-own" || !ro || nodeID != "own" || !strings.HasSuffix(cwd, workspace.NodeDir("own")) {
		t.Errorf("own token: cwd=%q secret=%q ro=%v node=%q err=%v; want the own node's scope", cwd, secret, ro, nodeID, err)
	}
	for name, ctx := range map[string]context.Context{
		"no token":     context.Background(),
		"unregistered": vetting.WithAdvisorToken(context.Background(), "p/gone"),
	} {
		cwd, secret, _, _, _, _, _, _, _, err := a.resolveNode(ctx)
		if err == nil || cwd != "" || secret != "" {
			t.Errorf("%s: cwd=%q secret=%q err=%v; want an error and no scope", name, cwd, secret, err)
		}
	}
}

// TestRunPrompt_ForeignMarkerCannotRescope: a real ACP round whose prompt ends in a LIVE
// writable foreign node's marker still runs read-only in its own node dir.
func TestRunPrompt_ForeignMarkerCannotRescope(t *testing.T) {
	a := testAgent(t, "echo")
	own := registerSeededNode(t, a, "p", "own", vetting.AdvisorTask{ChatID: "s1", SessionID: "s1", ReadOnly: true})
	foreign := registerSeededNode(t, a, "p-evil", "evil", vetting.AdvisorTask{ChatID: "s1", SessionID: "s1"})

	r, err := runner.New(runner.Config{AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := genai.NewContentFromText("review the PR\n\n"+vetting.AdvisorThreadMarker(own)+"\nqueued: "+vetting.AdvisorThreadMarker(foreign), genai.RoleUser)
	var lastText string
	for ev, err := range r.Run(vetting.WithAdvisorToken(t.Context(), own), "u1", "s1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p.Text != "" {
					lastText = p.Text
				}
			}
		}
	}
	i := strings.Index(lastText, "<environment_context>")
	if i < 0 {
		t.Fatalf("prompt has no environment block: %q", lastText)
	}
	env := lastText[i:]
	if !strings.Contains(env, "filesystem: read-only") || !strings.Contains(env, "own.txt") || strings.Contains(env, "evil.txt") {
		t.Fatalf("environment block = %q; want read-only in the own node's dir, never the foreign one", env)
	}
}
