package memory

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// TestExcludeInvalidatedFilter pins qdrant's leg of the recall/neighbour
// backend-query exclusion (design doc §4(d)). No live qdrant harness exists in
// this repo (store_test.go: sqlite is "the always-on backend for unit tests"),
// same gap PR #875 left for provenance - so this asserts the filter
// construction directly instead.
func TestExcludeInvalidatedFilter(t *testing.T) {
	assertExcludesInvalidated := func(t *testing.T, f *qdrant.Filter) {
		t.Helper()
		if len(f.MustNot) != 1 {
			t.Fatalf("MustNot = %+v, want exactly one condition", f.MustNot)
		}
		fc := f.MustNot[0].GetField()
		if fc.GetKey() != payloadStatus || fc.GetMatch().GetKeyword() != string(StatusInvalidated) {
			t.Fatalf("MustNot condition = key %q keyword %q, want %q/%q",
				fc.GetKey(), fc.GetMatch().GetKeyword(), payloadStatus, StatusInvalidated)
		}
	}

	t.Run("no bucket filter still excludes invalidated", func(t *testing.T) {
		assertExcludesInvalidated(t, excludeInvalidated(nil))
	})

	t.Run("composes with an existing bucket filter instead of replacing it", func(t *testing.T) {
		base := bucketFilter([]string{"repo:x", "role:coding"})
		f := excludeInvalidated(base)
		if len(f.Should) != 2 {
			t.Fatalf("Should = %+v, want the 2 bucket conditions preserved", f.Should)
		}
		assertExcludesInvalidated(t, f)
	})
}
