package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// gatedCtx is a worker's tool-call context INSIDE a gated DAG node: a durable
// session state (the `cd` cwd) plus the advisor-thread marker in the prompt -
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
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: chatID, SessionID: chatID, NodeID: nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	return &gatedCtx{fakeCtx: *newFakeCtx(), prompt: "do the task\n\n" + vetting.AdvisorThreadMarker(token)}
}

// TestConcurrentNodesEachSeeOnlyTheirOwnClone is the bug: two nodes of the SAME
// plan run concurrently in the SAME chat, each clones a DIFFERENT repo, and each
// must see ONLY its own clone. Before per-node scoping both clones landed in the
// one per-chat dir, so `list_dir .` showed both - and a research node happily
// read the other node's repo (live: the OpenHands explorer grepping goose's src).
func TestConcurrentNodesEachSeeOnlyTheirOwnClone(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}

	gooseCtx := newGatedCtx(t, "plan-1", "goose_research", "chat-1")
	ohCtx := newGatedCtx(t, "plan-1", "openhands_research", "chat-1")

	// Each node gets its own tree under its OWN scope dir - what a clone made by
	// the node's external (ACP) worker looks like to the surviving read tools.
	if err := seedNodeFile(fb, gooseCtx, "goose/README.md", "# goose"); err != nil {
		t.Fatalf("goose clone: %v", err)
	}
	if err := seedNodeFile(fb, ohCtx, "openhands/README.md", "# openhands"); err != nil {
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
			t.Errorf("list_dir . = %v: node can see ANOTHER node's clone %q - this is the correctness bug", names, tc.deny)
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

	// "/" is the node's OWN root, not the chat root: it is NOT a way out into a
	// sibling's tree. (It used to be. Nothing used it, and it was the last path by
	// which one node could read another's clone - see the sandbox's OS boundary,
	// which stops a run_command child doing the same thing.)
	if _, err := fb.withCwd(gooseCtx).readFile(readFileArgs{Path: "/openhands_research/openhands/README.md"}); err == nil {
		t.Error("a \"/\"-prefixed path reached a SIBLING node's clone; \"/\" must mean the node's own root")
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

// seedNodeFile writes content under the node scope the ctx's advisor-thread
// marker names - standing in for the clone the node's external worker makes.
func seedNodeFile(b fsBinding, ctx *gatedCtx, rel, content string) error {
	scoped := b.withCwd(ctx)
	real, err := scoped.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return err
	}
	return os.WriteFile(real, []byte(content), 0o644)
}

// firstComponent returns the first path segment of a slash path ("" for the
// root) - the immediate-child dir a listing entry sits in. (Was cd.go's helper;
// the cd tool is gone, the test invariant is not.)
func firstComponent(rel string) string {
	if rel == "" || rel == "." {
		return ""
	}
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}
