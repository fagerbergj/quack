package memory

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// TestExcludeInvalidatedFilter pins qdrant's leg of the recall/neighbour backend-query
// exclusion (design doc §4(d)) at the filter-construction level - cheaper than a live
// round-trip for this one. The live qdrant harness (qdranttest_test.go, #1268) covers the end-to-end recall exclusion instead (see TestInvalidateByID_HumanDelete/TestListIsNotSearch).
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
