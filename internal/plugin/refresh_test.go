package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, dir string, body string) string {
	t.Helper()
	p := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A missing tree must be reported, never fatal: skills changing how agents plan
// is bad, losing the server because GitHub is unreachable is worse.
func TestRefreshMissingTreeIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	m := writeManifest(t, dir, "plugins:\n  - name: demo\n    url: https://example.invalid/x\n    ref: abc123\n    path: "+filepath.Join(dir, "nope")+"\n")

	revs := Refresh(m, "")
	if len(revs) != 1 {
		t.Fatalf("want 1 revision, got %d", len(revs))
	}
	if revs[0].Head != "" {
		t.Errorf("missing tree should have empty Head, got %q", revs[0].Head)
	}
	if got := Summary(revs); got != "demo@missing" {
		t.Errorf("Summary = %q, want demo@missing", got)
	}
}

// The on-disk revision is what a run actually used, so it is read from the
// stamp rather than assumed to equal the pin (a branch pin never will).
func TestRefreshReportsOnDiskRevisionNotThePin(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "demo")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".plugin-ref"), []byte("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := writeManifest(t, dir, "plugins:\n  - name: demo\n    url: https://example.invalid/x\n    ref: main\n    path: "+tree+"\n")

	revs := Refresh(m, "")
	if len(revs) != 1 || revs[0].Head != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("got %+v", revs)
	}
	if revs[0].Ref != "main" {
		t.Errorf("Ref = %q, want the manifest pin", revs[0].Ref)
	}
	if got := Summary(revs); got != "demo@deadbee" {
		t.Errorf("Summary = %q", got)
	}
}

func TestRefreshUnreadableManifestIsNotFatal(t *testing.T) {
	if revs := Refresh(filepath.Join(t.TempDir(), "absent.yaml"), ""); revs != nil {
		t.Errorf("want nil for an unreadable manifest, got %+v", revs)
	}
}

// The real manifest must parse with the same shape scripts/plugins.sh expects.
func TestRefreshParsesTheRealManifest(t *testing.T) {
	entries, err := parseManifest(filepath.Join(repoRoot(t), ".agents", "vendor", "plugins.yaml"))
	if err != nil {
		t.Fatalf("real manifest did not parse: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("real manifest has no plugins")
	}
	for _, e := range entries {
		if e.url == "" || e.ref == "" || e.path == "" {
			t.Errorf("entry %q incomplete: %+v", e.name, e)
		}
	}
}

// An annotated pin (`ref: <sha>   # v4.9.0`) must compare equal to the head it
// names; otherwise every boot reports drift for a tree that is exactly on pin.
func TestParseManifestStripsInlineComments(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "demo")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	const sha = "0a4dd63ad4541f4f655c4108a295916f3c1d8fda"
	if err := os.WriteFile(filepath.Join(tree, ".plugin-ref"), []byte(sha+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := writeManifest(t, dir, "plugins:\n"+
		"  # a full-line comment, indented\n"+
		"  - name: demo\n    url: https://example.invalid/x#frag\n"+
		"    ref: "+sha+"   # v4.9.0\n    path: "+tree+"\n")

	entries, err := parseManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ref != sha {
		t.Errorf("ref = %q, want the bare sha", entries[0].ref)
	}
	if entries[0].url != "https://example.invalid/x#frag" {
		t.Errorf("url = %q, want the fragment preserved", entries[0].url)
	}
	revs := Refresh(m, "")
	if len(revs) != 1 || revs[0].Head != revs[0].Ref {
		t.Errorf("annotated pin reported as drift: ref=%q head=%q", revs[0].Ref, revs[0].Head)
	}
}
