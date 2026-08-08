package workflowcatalog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// writePlanWork lays down a minimal plan-work skill whose body carries a
// "Common workflows" table shaped like the real skills/plan-work/SKILL.md -
// header, separator, one shipped row, then trailing prose.
func writePlanWork(t *testing.T, dir string) skill.Source {
	t.Helper()
	d := filepath.Join(dir, "plan-work")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: plan-work\ndescription: test\n---\n\n" +
		"## Common workflows\n\n" +
		"| Request | DAG shape |\n" +
		"| --- | --- |\n" +
		"| Single information topic | ONE `web-researcher` node, no synthesizer |\n\n" +
		"**When to add a synthesizer.** ...\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return skill.NewFileSystemSource(os.DirFS(dir))
}

// TestWrapNoShapesIsIdentity is issue #805 test case 2: a deployment with no
// custom shapes must produce a catalog byte-identical to today's - Wrap must
// return the exact same Source, not a passthrough wrapper around it.
func TestWrapNoShapesIsIdentity(t *testing.T) {
	src := writePlanWork(t, t.TempDir())
	wrapped := Wrap(src, nil)
	want, err := src.LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	got, err := wrapped.LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("wrapped instructions changed with zero shapes:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestWrapAddsShapeToTable is issue #805 test case 1: a configured shape
// appears in the composed catalog as a new row of the SAME table.
func TestWrapAddsShapeToTable(t *testing.T) {
	src := writePlanWork(t, t.TempDir())
	shapes := []Shape{{
		Name: "document-ingest", Trigger: "Ingest a document into the knowledge base",
		DAGShape: "ONE `document-classifier` node (terminal)",
		Source:   "operator", Version: "abc123", Approved: true,
	}}
	got, err := Wrap(src, shapes).LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	want := "| Ingest a document into the knowledge base | ONE `document-classifier` node (terminal) |"
	if !strings.Contains(got, want) {
		t.Errorf("composed instructions missing new row %q:\n%s", want, got)
	}
	// The new row must land directly under the shipped row - still one
	// contiguous table, not a second table the planner might never read.
	shipped := "| Single information topic | ONE `web-researcher` node, no synthesizer |"
	lines := strings.Split(got, "\n")
	shippedIdx, wantIdx := -1, -1
	for i, l := range lines {
		if l == shipped {
			shippedIdx = i
		}
		if l == want {
			wantIdx = i
		}
	}
	if shippedIdx == -1 || wantIdx != shippedIdx+1 {
		t.Errorf("new row is not the line directly under the shipped row (shipped at %d, new at %d):\n%s", shippedIdx, wantIdx, got)
	}
}

// TestWrapCollisionSkipsShape proves the collision decision: a shape whose
// trigger matches an existing row (shipped or already-added) is refused
// deterministically, never left to "whichever the model reads first".
func TestWrapCollisionSkipsShape(t *testing.T) {
	src := writePlanWork(t, t.TempDir())
	shapes := []Shape{{
		Name: "dup", Trigger: "Single information topic", // collides with the shipped row verbatim
		DAGShape: "something else entirely",
	}}
	got, err := Wrap(src, shapes).LoadInstructions(context.Background(), "plan-work")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "something else entirely") {
		t.Errorf("colliding shape was composed in, want it refused:\n%s", got)
	}
	if strings.Count(got, "Single information topic") != 1 {
		t.Errorf("shipped row duplicated or lost:\n%s", got)
	}
}

// TestWrapOnlyAugmentsPlanWork proves other skills pass through unchanged.
func TestWrapOnlyAugmentsPlanWork(t *testing.T) {
	dir := t.TempDir()
	writePlanWork(t, dir)
	other := filepath.Join(dir, "format-markdown")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: format-markdown\ndescription: test\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	src := skill.NewFileSystemSource(os.DirFS(dir))
	shapes := []Shape{{Name: "x", Trigger: "t", DAGShape: "s"}}
	got, err := Wrap(src, shapes).LoadInstructions(context.Background(), "format-markdown")
	if err != nil {
		t.Fatal(err)
	}
	if got != "\nBody.\n" {
		t.Errorf("format-markdown instructions changed: %q", got)
	}
}
