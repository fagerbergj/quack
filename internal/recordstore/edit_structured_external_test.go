// edit_structured_external_test.go lives in package recordstore_test (not
// recordstore) so it can import vetting/dag - real callers of Client.Edit -
// and prove the fix against their actual registered Kinds, not just the package-internal test.structured stand-in.
package recordstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

func newClient(t *testing.T) *recordstore.Client {
	t.Helper()
	return recordstore.New(artifact.InMemoryService(), "quack", "user1", "chat1")
}

// TestEditArtifactStructuredFields is the regression test for the reported
// bug: edit_artifact did a raw byte search/replace on the serialized JSON, so
// a New containing a raw newline, quote, or backslash corrupted the record. Every Kind with a write_* tool shares this one Edit path (Client.Edit -> tryEdit -> applyStructuredEdits for Class == Structured), so one table covering a few real Kinds proves the fix generally, not per-agent.
func TestEditArtifactStructuredFields(t *testing.T) {
	ctx := context.Background()

	t.Run("code_review summary with newlines and a quote", func(t *testing.T) {
		c := newClient(t)
		rec := vetting.CodeReviewRecord{
			Verdict: "request_changes", Summary: "old summary",
			FindingIDs: []string{}, Dismissed: []vetting.DismissedEntry{}, Clean: []string{},
		}
		id, rev, err := c.SaveStructured(ctx, "code_review", rec, vetting.SubjectHint("chat-a"), recordstore.Lineage{})
		if err != nil {
			t.Fatal(err)
		}
		newSummary := "Line one.\nLine two has a \"quote\" and a backslash \\.\nLine three."
		gotRev, merged, err := c.Edit(ctx, id, rev, []recordstore.EditOp{{Old: "old summary", New: newSummary}}, recordstore.Lineage{})
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if gotRev != rev+1 {
			t.Fatalf("revision = %d, want %d", gotRev, rev+1)
		}
		var got vetting.CodeReviewRecord
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("merged content is not valid JSON: %v\n%s", err, merged)
		}
		if got.Summary != newSummary {
			t.Fatalf("Summary = %q, want %q", got.Summary, newSummary)
		}
	})

	t.Run("dag_plan nested array leaf", func(t *testing.T) {
		c := newClient(t)
		p := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{
			{NodeID: "coder-1", Task: "implement the fix"},
			{NodeID: "reviewer-1", Task: "review the fix"},
		}}
		id, rev, err := c.SaveStructured(ctx, "dag_plan", p, "", recordstore.Lineage{})
		if err != nil {
			t.Fatal(err)
		}
		newTask := "implement the fix\nand add a test"
		gotRev, merged, err := c.Edit(ctx, id, rev, []recordstore.EditOp{{Old: "implement the fix", New: newTask}}, recordstore.Lineage{})
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if gotRev != rev+1 {
			t.Fatalf("revision = %d, want %d", gotRev, rev+1)
		}
		var got dag.DagPlanRecord
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("merged content is not valid JSON: %v\n%s", err, merged)
		}
		if got.Assignments[0].Task != newTask {
			t.Fatalf("Assignments[0].Task = %q, want %q", got.Assignments[0].Task, newTask)
		}
		if got.Assignments[1].Task != "review the fix" {
			t.Fatalf("Assignments[1].Task changed unexpectedly: %q", got.Assignments[1].Task)
		}
	})

	t.Run("markdown blob keeps raw byte semantics", func(t *testing.T) {
		c := newClient(t)
		id, rev, err := c.SaveBlob(ctx, "text", []byte("# Title\n\nsome body text"), "text/markdown", "doc:1", recordstore.Lineage{})
		if err != nil {
			t.Fatal(err)
		}
		gotRev, merged, err := c.Edit(ctx, id, rev, []recordstore.EditOp{{Old: "some body text", New: "new body\ntext"}}, recordstore.Lineage{})
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if gotRev != rev+1 {
			t.Fatalf("revision = %d, want %d", gotRev, rev+1)
		}
		if want := "# Title\n\nnew body\ntext"; string(merged) != want {
			t.Fatalf("merged = %q, want %q", merged, want)
		}
	})

	t.Run("ambiguous match across two leaves conflicts", func(t *testing.T) {
		c := newClient(t)
		p := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{
			{NodeID: "n1", Task: "same text"},
			{NodeID: "n2", Task: "same text"},
		}}
		id, rev, err := c.SaveStructured(ctx, "dag_plan", p, "", recordstore.Lineage{})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.Edit(ctx, id, rev, []recordstore.EditOp{{Old: "same text", New: "changed"}}, recordstore.Lineage{})
		var conflict *recordstore.EditConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("Edit = %v, want *EditConflict for a match in two leaves", err)
		}
	})

	t.Run("old spanning a key and a value never matches", func(t *testing.T) {
		c := newClient(t)
		rec := vetting.CodeReviewRecord{
			Verdict: "approve", Summary: "hello",
			FindingIDs: []string{}, Dismissed: []vetting.DismissedEntry{}, Clean: []string{},
		}
		id, rev, err := c.SaveStructured(ctx, "code_review", rec, vetting.SubjectHint("chat-b"), recordstore.Lineage{})
		if err != nil {
			t.Fatal(err)
		}
		// This raw byte run exists in the serialized JSON (`"summary":"hello"`)
		// but spans a key and a value, never one decoded leaf - must count as
		// no match, not the raw-byte hit the old byte-level path would find.
		_, _, err = c.Edit(ctx, id, rev, []recordstore.EditOp{{Old: `"summary":"hello"`, New: `"summary":"world"`}}, recordstore.Lineage{})
		var conflict *recordstore.EditConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("Edit = %v, want *EditConflict for an old string spanning a key and a value", err)
		}
	})
}
