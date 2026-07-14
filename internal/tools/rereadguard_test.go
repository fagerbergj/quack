package tools

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/workspace"
)

// A node stuck re-reading the same file must be broken out of the loop and told to act.
//
// THE LIVE FAILURE (code-mode dogfood, 2026-07-13): a code-implementer ran 25 minutes,
// made 98 tool calls, and wrote NOTHING. 41% of its calls were repeats — it read
// internal/tools/registry.go TEN times. Its session had reached ~166,000 tokens against
// a 65,536-token window, so compaction summarised away each read as soon as it landed.
// Read, forget, re-read, forget. It could never hold enough context to start writing.
func TestRepeatedReadOfAnUnchangedFileIsRefused(t *testing.T) {
	b, ctx := readGuardBinding(t)
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "registry.go", Content: "package tools\n"}); err != nil {
		t.Fatal(err)
	}

	// The first maxSameReads reads are honoured — re-reading a couple of times is normal.
	for i := 1; i <= maxSameReads; i++ {
		if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "registry.go"}); err != nil {
			t.Fatalf("read %d/%d was refused, but a node must be able to re-read a file a few times: %v",
				i, maxSameReads, err)
		}
	}

	// The next one is the thrash. Refuse it, and say what to do instead.
	_, err := b.withCwd(ctx).readFile(readFileArgs{Path: "registry.go"})
	if err == nil {
		t.Fatal("the 4th identical read was served — the node stays in its read/forget loop and never writes")
	}
	msg := err.Error()
	for _, want := range []string{"edit_file", "offset", "grep"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name a way OUT (%q); it is an instruction, not a diagnostic.\ngot: %s", want, msg)
		}
	}
}

// Re-reading a file you just EDITED is exactly right, and must never be blocked. The
// guard is keyed on content, so a changed file starts over.
func TestReadingAChangedFileIsAlwaysAllowed(t *testing.T) {
	b, ctx := readGuardBinding(t)
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "x.go", Content: "v1\n"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSameReads+2; i++ {
		// Edit, then read back — the loop a verifying implementer runs constantly.
		if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "x.go", Content: strings.Repeat("v", i+1) + "\n"}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "x.go"}); err != nil {
			t.Fatalf("read %d after an edit was refused — a node must always be able to verify its own write: %v", i, err)
		}
	}
}

// Two different files, and two different nodes, never share a count.
func TestReadGuardIsPerNodeAndPerFile(t *testing.T) {
	b, ctx := readGuardBinding(t)
	for _, p := range []string{"a.go", "b.go"} {
		if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: p, Content: "same\n"}); err != nil {
			t.Fatal(err)
		}
	}
	// Exhaust a.go...
	for i := 0; i <= maxSameReads; i++ {
		_, _ = b.withCwd(ctx).readFile(readFileArgs{Path: "a.go"})
	}
	// ...b.go is untouched (identical CONTENT, different path).
	if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "b.go"}); err != nil {
		t.Fatalf("a different file was blocked by another file's count: %v", err)
	}
	// ...and so is another node's read of a.go.
	other := newGatedCtx(t, "plan-1", "other-node", "chat-1")
	if _, err := b.withCwd(other).writeFile(writeFileArgs{Path: "a.go", Content: "same\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.withCwd(other).readFile(readFileArgs{Path: "a.go"}); err != nil {
		t.Fatalf("a sibling node was blocked by this node's read count: %v", err)
	}
}

func readGuardBinding(t *testing.T) (fsBinding, agent.Context) {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps(), reads: newReadTracker()}
	return b, newGatedCtx(t, "plan-1", "implement-code-mode", "chat-1")
}
