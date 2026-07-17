package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// newTestBinding builds an fsBinding rooted at a fresh temp jail for userID,
// with default caps unless overridden.
func newTestBinding(t *testing.T, userID string) fsBinding {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	return fsBinding{userID: userID, jail: j, caps: workspace.DefaultCaps()}
}

func writeUserFile(t *testing.T, b fsBinding, relPath, content string) {
	t.Helper()
	real, err := b.jail.Resolve(b.userID, "", relPath)
	if err != nil {
		t.Fatalf("resolve %q: %v", relPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

func TestReadFileBasic(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "hello.txt", "line0\nline1\nline2")
	res, err := b.readFile(readFileArgs{Path: "hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "line0\nline1\nline2" {
		t.Errorf("Content = %q", res.Content)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if res.TotalLines != 3 {
		t.Errorf("TotalLines = %d, want 3", res.TotalLines)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "hello.txt", "l0\nl1\nl2\nl3\nl4")
	res, err := b.readFile(readFileArgs{Path: "hello.txt", Offset: 1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "l1\nl2" {
		t.Errorf("Content = %q, want l1\\nl2", res.Content)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true (more lines exist past the window)")
	}
	// A truncated read must hand back the exact next window offset so the model
	// pages forward instead of re-reading offset 0 (the loop this prevents).
	if res.NextOffset != 3 {
		t.Errorf("NextOffset = %d, want 3 (offset 1 + limit 2)", res.NextOffset)
	}
	if res.TotalLines != 5 {
		t.Errorf("TotalLines = %d, want 5", res.TotalLines)
	}
}

func TestReadFileOversizedTruncatesNotErrors(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.MaxReadBytes = 10 // tiny cap to force truncation deterministically
	writeUserFile(t, b, "big.txt", strings.Repeat("x", 1000))
	res, err := b.readFile(readFileArgs{Path: "big.txt"})
	if err != nil {
		t.Fatalf("read_file must never error on an oversized file, got: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true for an oversized read")
	}
	if len(res.Content) > 10 {
		t.Errorf("Content is %d bytes, want <= cap (10)", len(res.Content))
	}
}

func TestReadFileBinaryRejected(t *testing.T) {
	b := newTestBinding(t, "u1")
	real, err := b.jail.Resolve(b.userID, "", "bin.dat")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("abc\x00def"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readFile(readFileArgs{Path: "bin.dat"}); err == nil {
		t.Fatal("expected an error reading a binary file")
	}
}

func TestReadFileRejectsEscape(t *testing.T) {
	b := newTestBinding(t, "u1")
	if _, err := b.readFile(readFileArgs{Path: "../escape.txt"}); err == nil {
		t.Fatal("expected an escape error")
	}
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

func TestWriteFileCreatesAndOverwrites(t *testing.T) {
	b := newTestBinding(t, "u1")
	res, err := b.writeFile(writeFileArgs{Path: "a/b/c.txt", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Bytes != 5 {
		t.Errorf("first write: created=%v bytes=%d, want true/5", res.Created, res.Bytes)
	}
	res2, err := b.writeFile(writeFileArgs{Path: "a/b/c.txt", Content: "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created {
		t.Error("second write: created = true, want false (overwrite)")
	}
	if res2.Bytes != 11 {
		t.Errorf("second write: bytes = %d, want 11", res2.Bytes)
	}
}

func TestWriteFileOversizedErrors(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.MaxWriteBytes = 4
	if _, err := b.writeFile(writeFileArgs{Path: "f.txt", Content: "way too big"}); err == nil {
		t.Fatal("expected an error for oversized write")
	}
}

func TestWriteFileRejectsEscape(t *testing.T) {
	b := newTestBinding(t, "u1")
	// A `..` climb out of the jail is still rejected.
	if _, err := b.writeFile(writeFileArgs{Path: "../escape.txt", Content: "x"}); err == nil {
		t.Fatal("expected an escape error for a `..` climb")
	}
	// A leading "/" is now the jail-ROOT-relative escape hatch (see joinCwd), not
	// an OS-absolute path: it resolves INSIDE the jail (<root>/etc/passwd), so it
	// writes a contained file rather than escaping.
	if _, err := b.writeFile(writeFileArgs{Path: "/etc/passwd", Content: "x"}); err != nil {
		t.Fatalf("leading-slash path should resolve inside the jail, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// edit_file
// ---------------------------------------------------------------------------

func TestEditFileNoMatchErrors(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "package main\n")
	if _, err := b.editFile(editFileArgs{Path: "f.go", Old: "does not exist", New: "x"}); err == nil {
		t.Fatal("expected a no-match error")
	}
}

func TestEditFileMultiMatchWithoutReplaceAllErrors(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "foo\nfoo\nfoo\n")
	if _, err := b.editFile(editFileArgs{Path: "f.go", Old: "foo", New: "bar"}); err == nil {
		t.Fatal("expected a multi-match error without replace_all")
	}
}

func TestEditFileExactWhitespaceSingleReplacement(t *testing.T) {
	b := newTestBinding(t, "u1")
	original := "func f() {\n\tif true {\n\t\treturn\n\t}\n}\n"
	writeUserFile(t, b, "f.go", original)
	res, err := b.editFile(editFileArgs{
		Path: "f.go",
		Old:  "\tif true {\n\t\treturn\n\t}",
		New:  "\tif false {\n\t\treturn\n\t}",
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 1 {
		t.Errorf("Replacements = %d, want 1", res.Replacements)
	}
	real, _ := b.jail.Resolve(b.userID, "", "f.go")
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	want := "func f() {\n\tif false {\n\t\treturn\n\t}\n}\n"
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "foo\nfoo\nfoo\n")
	res, err := b.editFile(editFileArgs{Path: "f.go", Old: "foo", New: "bar", ReplaceAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Replacements != 3 {
		t.Errorf("Replacements = %d, want 3", res.Replacements)
	}
}

func TestEditFileNoMatchReportsNearMiss(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go",
		"package main\n\nfunc handleIssues() {\n\tswitch action {\n\tcase \"open\":\n\t\treturn nil\n\t}\n}\n")
	_, err := b.editFile(editFileArgs{
		Path: "f.go",
		Old:  "switch action {\n\tcase \"closed\":\n\t\treturn nil\n\t}",
		New:  "x",
	})
	if err == nil {
		t.Fatal("expected a no-match error")
	}
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error = %q, want it to name line 4 (the near-miss)", err.Error())
	}
	if !strings.Contains(err.Error(), `switch action {`) {
		t.Errorf("error = %q, want it to quote the near-miss line", err.Error())
	}
}

func TestEditFileNoMatchNoNearMissStaysBounded(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "package main\n\nfunc f() {}\n")
	_, err := b.editFile(editFileArgs{Path: "f.go", Old: "totally unrelated text", New: "x"})
	if err == nil {
		t.Fatal("expected a no-match error")
	}
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("error = %q, want the bounded fallback (no near-miss exists)", err.Error())
	}
	if !strings.Contains(err.Error(), "grep") {
		t.Errorf("error = %q, want it to point at grep", err.Error())
	}
}

func TestEditFileOuterWhitespaceRetry(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "func f() {\n    return 1\n}\n")
	res, err := b.editFile(editFileArgs{
		Path: "f.go",
		Old:  "  return 1  \n", // wrong indentation + stray outer whitespace
		New:  "  return 2  \n",
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.Replacements != 1 {
		t.Errorf("Replacements = %d, want 1", res.Replacements)
	}
	real, _ := b.jail.Resolve(b.userID, "", "f.go")
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	want := "func f() {\n    return 2\n}\n"
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEditFileMultiMatchListsLineNumbers(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.go", "foo\nbar\nfoo\nbaz\nfoo\n")
	_, err := b.editFile(editFileArgs{Path: "f.go", Old: "foo", New: "qux"})
	if err == nil {
		t.Fatal("expected a multi-match error without replace_all")
	}
	if !strings.Contains(err.Error(), "(lines 1, 3, 5)") {
		t.Errorf("error = %q, want it to list lines 1, 3, 5", err.Error())
	}
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

func TestListDirDepthAndCaps(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "top.txt", "x")
	writeUserFile(t, b, "sub/mid.txt", "x")
	writeUserFile(t, b, "sub/deep/leaf.txt", "x")

	res, err := b.listDir(listDirArgs{Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range res.Entries {
		paths = append(paths, e.Path)
	}
	for _, want := range []string{"top.txt", "sub"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("depth=1 listing %v missing %q", paths, want)
		}
	}
	for _, unwanted := range []string{"sub/mid.txt", "sub/deep", "sub/deep/leaf.txt"} {
		for _, p := range paths {
			if p == unwanted {
				t.Errorf("depth=1 listing should not include %q, got %v", unwanted, paths)
			}
		}
	}
}

func TestListDirCapTruncates(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.MaxListEntries = 2
	writeUserFile(t, b, "a.txt", "x")
	writeUserFile(t, b, "b.txt", "x")
	writeUserFile(t, b, "c.txt", "x")
	res, err := b.listDir(listDirArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(res.Entries) > 2 {
		t.Errorf("got %d entries, want <= 2", len(res.Entries))
	}
}

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

func TestGlobDoublestar(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "a.go", "x")
	writeUserFile(t, b, "sub/b.go", "x")
	writeUserFile(t, b, "sub/deep/c.go", "x")
	writeUserFile(t, b, "readme.md", "x")

	res, err := b.glob(globArgs{Pattern: "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 3 {
		t.Errorf("got %d matches, want 3: %v", len(res.Paths), res.Paths)
	}
	for _, p := range res.Paths {
		if !strings.HasSuffix(p, ".go") {
			t.Errorf("unexpected match %q", p)
		}
	}
}

func TestGlobResultCap(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.MaxResults = 2
	for i := 0; i < 5; i++ {
		writeUserFile(t, b, fmt.Sprintf("f/file%d.txt", i), "x")
	}
	res, err := b.glob(globArgs{Pattern: "f/*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(res.Paths) != 2 {
		t.Errorf("got %d paths, want 2", len(res.Paths))
	}
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

func TestGrepFindsMatches(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "a.go", "package main\nfunc main() {}\n")
	writeUserFile(t, b, "b.txt", "no matches here\n")

	res, err := b.grep(grepArgs{Pattern: "^func"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(res.Matches), res.Matches)
	}
	if res.Matches[0].Path != "a.go" || res.Matches[0].Line != 2 {
		t.Errorf("match = %+v, want a.go:2", res.Matches[0])
	}
}

func TestGrepResultCap(t *testing.T) {
	b := newTestBinding(t, "u1")
	b.caps.MaxResults = 2
	writeUserFile(t, b, "f.txt", "match\nmatch\nmatch\nmatch\n")
	res, err := b.grep(grepArgs{Pattern: "match"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(res.Matches) != 2 {
		t.Errorf("got %d matches, want 2", len(res.Matches))
	}
}

func TestGrepSkipsBinary(t *testing.T) {
	b := newTestBinding(t, "u1")
	real, err := b.jail.Resolve(b.userID, "", "bin.dat")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("match\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := b.grep(grepArgs{Pattern: "match"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 0 {
		t.Errorf("expected binary file to be skipped, got matches: %+v", res.Matches)
	}
}

// ---------------------------------------------------------------------------
// delete_path
// ---------------------------------------------------------------------------

func TestDeletePathFile(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "f.txt", "x")
	res, err := b.deletePath(deletePathArgs{Path: "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
	real, _ := b.jail.Resolve(b.userID, "", "f.txt")
	if _, statErr := os.Stat(real); !os.IsNotExist(statErr) {
		t.Error("file still exists after delete")
	}
}

func TestDeletePathNonEmptyDirRequiresRecursive(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "dir/f.txt", "x")
	if _, err := b.deletePath(deletePathArgs{Path: "dir"}); err == nil {
		t.Fatal("expected an error deleting a non-empty dir without recursive")
	}
	res, err := b.deletePath(deletePathArgs{Path: "dir", Recursive: true})
	if err != nil {
		t.Fatalf("recursive delete: %v", err)
	}
	if res.Deleted < 2 { // dir + f.txt
		t.Errorf("Deleted = %d, want >= 2", res.Deleted)
	}
	real, _ := b.jail.Resolve(b.userID, "", "dir")
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Error("dir still exists after recursive delete")
	}
}

func TestDeletePathEmptyDirNoRecursiveNeeded(t *testing.T) {
	b := newTestBinding(t, "u1")
	real, err := b.jail.Resolve(b.userID, "", "emptydir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := b.deletePath(deletePathArgs{Path: "emptydir"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
}

// ---------------------------------------------------------------------------
// newFSBinding / registry
// ---------------------------------------------------------------------------

func TestNewFSBindingRequiresWorkspace(t *testing.T) {
	if _, err := newFSBinding(Deps{}); err == nil {
		t.Fatal("expected an error when Deps.Workspace is nil")
	}
}

func TestNewFSBindingDefaultsCaps(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := newFSBinding(Deps{Workspace: j, WorkspaceUserID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.caps, workspace.DefaultCaps()) {
		t.Errorf("caps = %+v, want DefaultCaps()", b.caps)
	}
}

func TestFSToolsRegistered(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "delete_path"}
	built, err := Build(names, Deps{Workspace: j, WorkspaceUserID: "local"})
	if err != nil {
		t.Fatalf("Build(%v): %v", names, err)
	}
	if len(built) != len(names) {
		t.Fatalf("Build returned %d tools, want %d", len(built), len(names))
	}
	for i, tl := range built {
		if tl.Name() != names[i] {
			t.Errorf("tool[%d].Name() = %q, want %q", i, tl.Name(), names[i])
		}
	}
}

func TestFSToolsWithoutWorkspaceError(t *testing.T) {
	if _, err := Build([]string{"read_file"}, Deps{}); err == nil {
		t.Fatal("expected an error building read_file without a workspace jail configured")
	}
}
