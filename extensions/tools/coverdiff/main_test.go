package main

import (
	"testing"
)

// diff/profile key alignment is the load-bearing part of the gate: a
// mismatch is a silent no-op (every PR passes).
func TestChangedFilesProfileKeyAlignment(t *testing.T) {
	diff := `diff --git a/sdk/sdk.go b/sdk/sdk.go
--- a/sdk/sdk.go
+++ b/sdk/sdk.go
@@ -566,0 +567,8 @@
+// probe
+func Probe(x int) string {
+	if x > 0 {
+		return "pos"
+	}
+	return "neg"
+}
+
diff --git a/sdk/sdk_test.go b/sdk/sdk_test.go
--- a/sdk/sdk_test.go
+++ b/sdk/sdk_test.go
@@ -1,0 +2,3 @@
+func TestProbe(t *testing.T) {
+	_ = Probe(1)
+}
`
	changed := changedFiles(diff, "sdk")
	// The profile records paths without the .go suffix (regex capture);
	// the diff side must key the same way or nothing ever matches.
	rs, ok := changed["sdk/sdk"]
	if !ok {
		t.Fatalf("changed map has no profile-aligned key: got %v", changed)
	}
	for _, ln := range []int{567, 568, 569, 570, 571, 572, 573, 574} {
		if !rs[ln] {
			t.Fatalf("line %d not marked changed", ln)
		}
	}
	if rs[566] || rs[575] {
		t.Fatal("out-of-hunk lines marked changed")
	}
	if _, ok := changed["sdk/sdk_test"]; ok {
		t.Fatal("_test.go must be excluded from the gated set")
	}
}

func TestRelRepoPath(t *testing.T) {
	got := relRepoPath("github.com/fagerbergj/quack-extensions/github/app")
	if got != "github/app" {
		t.Fatalf("relRepoPath = %q, want github/app", got)
	}
}
