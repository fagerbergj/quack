package tools

import (
	"path/filepath"
	"testing"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// gatedCtx is a worker's tool-call context INSIDE a gated DAG node: a durable
// session state (the `cd` cwd) plus the advisor-thread marker in the prompt —
// the one identity channel the workspace tools recover the chat scope AND the
// node scope from (see scopeFromContext).
type gatedCtx struct {
	fakeCtx
	prompt string
}

func (c *gatedCtx) UserContent() *genai.Content {
	return &genai.Content{Parts: []*genai.Part{{Text: c.prompt}}}
}

// newGatedCtx registers a node's advisor thread (as dag.newGatedNode does at
// node entry) and returns the tool context a worker in that node calls with.
func newGatedCtx(t *testing.T, planID, nodeID, chatID string) *gatedCtx {
	t.Helper()
	token := vetting.AdvisorThreadToken(planID, nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{SessionID: chatID, NodeID: nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	return &gatedCtx{fakeCtx: *newFakeCtx(), prompt: "do the task\n\n" + vetting.AdvisorThreadMarker(token)}
}

// TestConcurrentNodesEachSeeOnlyTheirOwnClone is the bug: two nodes of the SAME
// plan run concurrently in the SAME chat, each clones a DIFFERENT repo, and each
// must see ONLY its own clone. Before per-node scoping both clones landed in the
// one per-chat dir, so `list_dir .` showed both — and a research node happily
// read the other node's repo (live: the OpenHands explorer grepping goose's src).
func TestConcurrentNodesEachSeeOnlyTheirOwnClone(t *testing.T) {
	requireGit(t)
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gb := gitBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}

	gooseBare := newBareRepoFixture(t)
	openhandsBare := newBareRepoFixture(t)

	gooseCtx := newGatedCtx(t, "plan-1", "goose_research", "chat-1")
	ohCtx := newGatedCtx(t, "plan-1", "openhands_research", "chat-1")

	// Each node clones its own repo, with a jail-relative dir exactly as the tool
	// would default it (the repo name) — resolved against the node's OWN cwd.
	if _, err := gb.withCwd(gooseCtx).cloneRepo("file://"+gooseBare, "goose", nil, ""); err != nil {
		t.Fatalf("goose clone: %v", err)
	}
	if _, err := gb.withCwd(ohCtx).cloneRepo("file://"+openhandsBare, "openhands", nil, ""); err != nil {
		t.Fatalf("openhands clone: %v", err)
	}

	listNames := func(ctx *gatedCtx) []string {
		t.Helper()
		res, err := fb.withCwd(ctx).listDir(listDirArgs{Path: "."})
		if err != nil {
			t.Fatalf("list_dir .: %v", err)
		}
		var names []string
		for _, e := range res.Entries {
			names = append(names, firstComponent(filepath.ToSlash(e.Path)))
		}
		return names
	}

	for _, tc := range []struct {
		ctx        *gatedCtx
		want, deny string
	}{
		{gooseCtx, "goose", "openhands"},
		{ohCtx, "openhands", "goose"},
	} {
		names := listNames(tc.ctx)
		var sawWant, sawDeny bool
		for _, n := range names {
			sawWant = sawWant || n == tc.want
			sawDeny = sawDeny || n == tc.deny
		}
		if !sawWant {
			t.Errorf("list_dir . = %v, want it to contain the node's own clone %q", names, tc.want)
		}
		if sawDeny {
			t.Errorf("list_dir . = %v: node can see ANOTHER node's clone %q — this is the correctness bug", names, tc.deny)
		}
	}

	// A relative read resolves under the node's own dir…
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "goose/README.md"}); err != nil {
		t.Errorf("relative read in own node dir: %v", err)
	}
	// …and cannot reach the sibling node's clone by a relative path.
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "openhands/README.md"}); err == nil {
		t.Error("a relative path reached another node's clone; the node dir must be the default scope")
	}

	// The escape hatch stays: a leading "/" addresses the CHAT root, so a
	// downstream node can still reach an upstream node's clone deliberately.
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "/openhands_research/openhands/README.md"}); err != nil {
		t.Errorf("chat-root escape hatch must still reach another node's clone: %v", err)
	}

	// Nothing escapes the jail, node dir or not.
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "../../../etc/passwd"}); err == nil {
		t.Error("a ../ climb escaped the jail")
	}

	// Everything a node wrote still lives under the chat scope, so deleting the
	// chat still deletes every node's work.
	if err := j.RemoveChatScope("u1", "chat-1"); err != nil {
		t.Fatalf("RemoveChatScope: %v", err)
	}
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "goose/README.md"}); err == nil {
		t.Error("RemoveChatScope left a node's dir behind")
	}
}

// TestCdComposesWithTheNodeDir: `cd` composes against the node's own dir, and a
// deliberate `cd /` (to the chat root) is NOT silently undone by the node
// default on the next tool call.
func TestCdComposesWithTheNodeDir(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	ctx := newGatedCtx(t, "plan-1", "node-1", "chat-1")

	// The node's own dir is the default cwd: a relative write lands in it.
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "notes.md", Content: "hi"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if got, err := b.withCwd(ctx).readFile(readFileArgs{Path: "/node-1/notes.md"}); err != nil || got.Content != "hi" {
		t.Fatalf("write_file did not land under the node dir: %v", err)
	}

	// cd to the chat root, then a relative read must resolve THERE (not silently
	// back under the node dir).
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "/"}); err != nil {
		t.Fatalf("cd /: %v", err)
	}
	if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "node-1/notes.md"}); err != nil {
		t.Errorf("after `cd /` a relative path must resolve against the chat root: %v", err)
	}
}
