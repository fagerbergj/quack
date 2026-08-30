package skillsource

import (
	"bytes"
	"context"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

//go:embed testdata
var testdataFS embed.FS

// embedSkillFS is rooted at testdata/, with embedskill/ as its one skill dir -
// go:embed keeps the "testdata" prefix, unlike os.DirFS(dir).
var embedSkillFS = mustSub(testdataFS, "testdata")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func TestNewFileSystemSource_UnknownFrontmatterFieldsLoad(t *testing.T) {
	body := "Do the thing.\n\n---\nfoo: bar\n---\nMore instructions.\n"
	content := "---\n" +
		"name: with-unknown\n" +
		"description: a skill with unknown fields\n" +
		"argument-hint: \"[lite|full|ultra]\"\n" +
		"unknown-field: whatever\n" +
		"---\n" + body

	t.Run("os.DirFS", func(t *testing.T) {
		dir := t.TempDir()
		skillDir := filepath.Join(dir, "with-unknown")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		src := NewFileSystemSource(os.DirFS(dir))
		checkLoads(t, src, "with-unknown", "a skill with unknown fields", body)
	})

	t.Run("embed.FS", func(t *testing.T) {
		src := NewFileSystemSource(embedSkillFS)
		checkLoads(t, src, "embedskill", "an embedded skill with an unknown field", "Embedded instructions.\n")
	})
}

func checkLoads(t *testing.T, src skill.Source, name, wantDesc, wantBody string) {
	t.Helper()
	fm, err := src.LoadFrontmatter(context.Background(), name)
	if err != nil {
		t.Fatalf("LoadFrontmatter(%q): %v", name, err)
	}
	if fm.Name != name {
		t.Errorf("Name = %q, want %q", fm.Name, name)
	}
	if fm.Description != wantDesc {
		t.Errorf("Description = %q, want %q", fm.Description, wantDesc)
	}
	ins, err := src.LoadInstructions(context.Background(), name)
	if err != nil {
		t.Fatalf("LoadInstructions(%q): %v", name, err)
	}
	if ins != wantBody {
		t.Errorf("instructions = %q, want %q", ins, wantBody)
	}
}

func TestFilterFrontmatter_BodyByteIdentical(t *testing.T) {
	body := "Some text.\n\n---\nfoo: bar\n---\n\nMore text with a foo: bar line too.\n"
	content := []byte("---\nname: x\ndescription: d\nunknown: y\n---\n" + body)

	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false for valid input")
	}
	gotBody := out[bytes.Index(out, []byte("---\n"))+4:]
	// Skip past the rewritten frontmatter block to the second "---\n".
	secondSep := bytes.Index(gotBody, []byte("---\n"))
	gotBody = gotBody[secondSep+4:]
	if string(gotBody) != body {
		t.Errorf("body = %q, want byte-identical %q", gotBody, body)
	}
	if bytes.Contains(out, []byte("unknown")) {
		t.Errorf("filtered output still contains dropped key: %s", out)
	}
}

func TestFilterFrontmatter_MultilineDescriptionSurvives(t *testing.T) {
	content := []byte("---\nname: x\ndescription: >\n  line one\n  line two\nunknown: drop-me\n---\nbody\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	fm, markdown, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered output): %v", err)
	}
	if fm.Description != "line one line two\n" {
		t.Errorf("Description = %q", fm.Description)
	}
	if markdown != "body\n" {
		t.Errorf("markdown = %q", markdown)
	}
}

func TestFilterFrontmatter_MalformedInputPassesThroughUnfiltered(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"no leading separator", "name: x\ndescription: d\n"},
		{"no closing separator", "---\nname: x\ndescription: d\n"},
		{"invalid yaml", "---\nname: [unterminated\n---\nbody\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := filterFrontmatter([]byte(tt.content))
			if ok {
				t.Errorf("filterFrontmatter(%q) ok = true, want false", tt.content)
			}
		})
	}
}

// TestFilterFrontmatter_MetadataScalarFidelity guards against the map
// round-trip corruption a plain map[string]any decode+remarshal caused: YAML
// implicit typing resolves ambiguous scalars ("007" -> int 7, "1.0" -> float
// 1) during a generic decode, and remarshaling writes back the resolved
// form. Ground truth is ADK's own single-hop decode of the same content with
// the unknown key already removed by hand.
func TestFilterFrontmatter_MetadataScalarFidelity(t *testing.T) {
	withUnknown := []byte("---\n" +
		"name: x\n" +
		"description: d\n" +
		"unknown-field: gone\n" +
		"metadata:\n" +
		"  version: 007\n" +
		"  ratio: 1.0\n" +
		"  quoted: \"1.0\"\n" +
		"---\nbody\n")
	groundTruth := []byte("---\n" +
		"name: x\n" +
		"description: d\n" +
		"metadata:\n" +
		"  version: 007\n" +
		"  ratio: 1.0\n" +
		"  quoted: \"1.0\"\n" +
		"---\nbody\n")

	wantFM, _, err := skill.ParseBytes(groundTruth)
	if err != nil {
		t.Fatalf("ParseBytes(groundTruth): %v", err)
	}

	out, ok := filterFrontmatter(withUnknown)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	gotFM, _, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered): %v", err)
	}
	if !reflect.DeepEqual(gotFM.Metadata, wantFM.Metadata) {
		t.Errorf("Metadata = %#v, want %#v (ground truth)", gotFM.Metadata, wantFM.Metadata)
	}
	if gotFM.Metadata["version"] != "007" {
		t.Errorf(`Metadata["version"] = %q, want "007"`, gotFM.Metadata["version"])
	}
	if gotFM.Metadata["ratio"] != "1.0" {
		t.Errorf(`Metadata["ratio"] = %q, want "1.0"`, gotFM.Metadata["ratio"])
	}
}

// TestFilterFrontmatter_AllKeysKnownByteIdentical proves the gate: when
// every frontmatter key is already known, filterFrontmatter must return the
// original bytes untouched rather than parse-and-rewrite.
func TestFilterFrontmatter_AllKeysKnownByteIdentical(t *testing.T) {
	content := []byte("---\nname: x\ndescription: d\nlicense: MIT\n---\nbody\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	if !bytes.Equal(out, content) {
		t.Errorf("out = %q, want byte-identical original %q", out, content)
	}
}

// TestFilterFrontmatter_CRLFWithUnknownKeyLoads proves the "\r\n" separator
// form (ADK's frontmatterSeparatorWin) is handled, not just "\n" - a CRLF
// skill with an unknown key must load instead of falling through unfiltered
// and getting skipped by Tolerant.
func TestFilterFrontmatter_CRLFWithUnknownKeyLoads(t *testing.T) {
	content := []byte("---\r\nname: x\r\ndescription: d\r\nunknown: y\r\n---\r\nbody\r\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false for CRLF input")
	}
	fm, _, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered CRLF output): %v (output: %q)", err, out)
	}
	if fm.Name != "x" {
		t.Errorf("Name = %q, want x", fm.Name)
	}
}

// TestFilterFrontmatter_ScalarEndingInSepIsNotAFalseBoundary proves the
// closing-separator search is line-anchored, not a substring search: a known
// key's plain-scalar value ending in "---" must not be mistaken for the
// closing "---\n" line. Before line-anchoring, this content's block was
// truncated at "bar---\n", corrupting description to "bar" and leaking the
// real "---\n" line into the markdown body.
func TestFilterFrontmatter_ScalarEndingInSepIsNotAFalseBoundary(t *testing.T) {
	content := []byte("---\nname: x\nunknown: y\ndescription: bar---\n---\nbody\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	fm, markdown, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered): %v (output: %q)", err, out)
	}
	if fm.Description != "bar---" {
		t.Errorf("Description = %q, want %q", fm.Description, "bar---")
	}
	if markdown != "body\n" {
		t.Errorf("markdown = %q, want %q", markdown, "body\n")
	}
}

// TestFilterFrontmatter_BlockScalarIndentedSepIsNotAFalseBoundary covers the
// same false-boundary bug for an indented "---" inside a folded/literal
// block scalar, alongside an unknown key.
func TestFilterFrontmatter_BlockScalarIndentedSepIsNotAFalseBoundary(t *testing.T) {
	content := []byte("---\nname: x\nunknown: y\ndescription: |\n  before\n  ---\n  after\n---\nbody\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	fm, markdown, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered): %v (output: %q)", err, out)
	}
	wantDesc := "before\n---\nafter\n"
	if fm.Description != wantDesc {
		t.Errorf("Description = %q, want %q", fm.Description, wantDesc)
	}
	if markdown != "body\n" {
		t.Errorf("markdown = %q, want %q", markdown, "body\n")
	}
}

// TestFilterFrontmatter_UnknownKeyAfterFalseBoundaryStillDropped proves the
// gate doesn't get fooled by a false boundary either: an unknown key sitting
// after a scalar value that contains "---" must still be found and dropped
// (not silently missed because the truncated scan only saw known keys
// before the false cut), and the skill must load.
func TestFilterFrontmatter_UnknownKeyAfterFalseBoundaryStillDropped(t *testing.T) {
	content := []byte("---\nname: x\ndescription: bar---\nunknown: y\n---\nbody\n")
	out, ok := filterFrontmatter(content)
	if !ok {
		t.Fatal("filterFrontmatter returned ok=false")
	}
	if bytes.Contains(out, []byte("unknown")) {
		t.Errorf("filtered output still contains dropped key: %s", out)
	}
	fm, markdown, err := skill.ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes(filtered): %v (output: %q)", err, out)
	}
	if fm.Description != "bar---" {
		t.Errorf("Description = %q, want %q", fm.Description, "bar---")
	}
	if markdown != "body\n" {
		t.Errorf("markdown = %q, want %q", markdown, "body\n")
	}
}

// TestTolerantStillSkipsMalformedSkill proves #1085's backstop still catches
// a genuinely broken SKILL.md (the filter only handles unknown keys).
func TestTolerantStillSkipsMalformedSkill(t *testing.T) {
	dir := t.TempDir()
	goodDir := filepath.Join(dir, "good")
	badDir := filepath.Join(dir, "bad")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "SKILL.md"), []byte("---\nname: good\ndescription: fine\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Missing closing separator: unparseable regardless of the filter.
	if err := os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("---\nname: bad\ndescription: broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := Tolerant(NewFileSystemSource(os.DirFS(dir)), os.DirFS(dir), dir)
	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	if got := names(fms); !got["good"] || got["bad"] {
		t.Errorf("ListFrontmatters names = %v, want good only", got)
	}
}

func TestFilterFrontmatterFS_ReadDirAndStatDelegate(t *testing.T) {
	t.Run("os.DirFS", func(t *testing.T) {
		dir := t.TempDir()
		skillDir := filepath.Join(dir, "s")
		if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: s\ndescription: d\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "references", "r.md"), []byte("ref"), 0o644); err != nil {
			t.Fatal(err)
		}
		src := NewFileSystemSource(os.DirFS(dir))
		res, err := src.ListResources(context.Background(), "s", "")
		if err != nil {
			t.Fatalf("ListResources: %v", err)
		}
		if len(res) != 1 || res[0] != "references/r.md" {
			t.Errorf("ListResources = %v, want [references/r.md]", res)
		}
	})

	t.Run("embed.FS", func(t *testing.T) {
		src := NewFileSystemSource(embedSkillFS)
		fms, err := src.ListFrontmatters(context.Background())
		if err != nil {
			t.Fatalf("ListFrontmatters: %v", err)
		}
		if got := names(fms); !got["embedskill"] {
			t.Errorf("ListFrontmatters names = %v, want embedskill", got)
		}
	})
}
