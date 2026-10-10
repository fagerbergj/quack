package skillsource

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"time"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"gopkg.in/yaml.v3"
)

// knownFrontmatterKeys is derived from skill.Frontmatter's yaml tags rather
// than hardcoded, so a future ADK field is picked up automatically.
var knownFrontmatterKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(skill.Frontmatter{})
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" {
			name = strings.ToLower(field.Name) // untagged field: yaml.v3's own default key
		}
		keys[name] = true
	}
	return keys
}()

// NewFileSystemSource filters frontmatter before ADK's strict (KnownFields) decoder, so a SKILL.md with a
// field ADK doesn't know (e.g. Claude Code's argument-hint) still loads.
func NewFileSystemSource(fsys fs.FS) skill.Source {
	return skill.NewFileSystemSource(filterFrontmatterFS{fsys})
}

// filterFrontmatterFS drops unknown top-level frontmatter keys from every
// SKILL.md it opens; every other file and fs.FS behavior passes through.
type filterFrontmatterFS struct{ fs.FS }

func (f filterFrontmatterFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil || path.Base(name) != "SKILL.md" {
		return file, err
	}
	defer func() { _ = file.Close() }()

	orig, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	filtered, ok := filterFrontmatter(orig)
	if !ok {
		filtered = orig // unparseable: pass through unchanged, Tolerant decides skip-vs-fatal
	}
	return &memFile{Reader: bytes.NewReader(filtered), size: int64(len(filtered))}, nil
}

// ReadDir/Stat delegate to the underlying fs.FS when it supports them, so
// fs.Sub/fs.WalkDir/ListResources behave exactly as before the wrap.
func (f filterFrontmatterFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if rd, ok := f.FS.(fs.ReadDirFS); ok {
		return rd.ReadDir(name)
	}
	return fs.ReadDir(f.FS, name)
}

func (f filterFrontmatterFS) Stat(name string) (fs.FileInfo, error) {
	if sf, ok := f.FS.(fs.StatFS); ok {
		return sf.Stat(name)
	}
	return fs.Stat(f.FS, name)
}

// lfSep/crlfSep mirror ADK's two accepted separator forms; a BOM prefix fails in ADK regardless.
var lfSep = []byte("---\n")
var crlfSep = []byte("---\r\n")

// leadingSep returns whichever separator form content starts with. A match
// at offset 0 is trivially line-anchored.
func leadingSep(content []byte) (sep []byte, ok bool) {
	if bytes.HasPrefix(content, crlfSep) {
		return crlfSep, true
	}
	if bytes.HasPrefix(content, lfSep) {
		return lfSep, true
	}
	return nil, false
}

// indexLineStart finds the first sep that starts a line. ADK anchors the closing separator to a whole line,
// so a mid-line "---" (in a scalar or block body) must not match.
func indexLineStart(b, sep []byte) int {
	off := 0
	for {
		i := bytes.Index(b[off:], sep)
		if i == -1 {
			return -1
		}
		idx := off + i
		if idx == 0 || b[idx-1] == '\n' {
			return idx
		}
		off = idx + 1
	}
}

// findSep returns the earliest line-anchored occurrence of either separator
// form in b.
func findSep(b []byte) (idx, seplen int, ok bool) {
	iLF := indexLineStart(b, lfSep)
	iCRLF := indexLineStart(b, crlfSep)
	switch {
	case iLF == -1 && iCRLF == -1:
		return 0, 0, false
	case iCRLF != -1 && (iLF == -1 || iCRLF <= iLF):
		return iCRLF, len(crlfSep), true
	default:
		return iLF, len(lfSep), true
	}
}

// filterFrontmatter drops unknown top-level frontmatter keys by editing the yaml.Node AST: a map round-trip
// rewrites scalars (`007` -> `7`). Returns content as-is if nothing is unknown; ok=false on invalid input.
func filterFrontmatter(content []byte) (out []byte, ok bool) {
	openSep, ok := leadingSep(content)
	if !ok {
		return nil, false
	}
	rest := content[len(openSep):]
	closeIdx, closeSepLen, ok := findSep(rest)
	if !ok {
		return nil, false
	}
	yamlBlock := rest[:closeIdx]
	closeSep := rest[closeIdx : closeIdx+closeSepLen]
	body := rest[closeIdx+closeSepLen:]

	var doc yaml.Node
	if err := yaml.Unmarshal(yamlBlock, &doc); err != nil {
		return nil, false
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false
	}
	mapping := doc.Content[0]

	kept := mapping.Content[:0]
	changed := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, val := mapping.Content[i], mapping.Content[i+1]
		if !knownFrontmatterKeys[key.Value] {
			changed = true
			continue
		}
		kept = append(kept, key, val)
	}
	if !changed {
		return content, true // every key known: original bytes, no rewrite
	}
	mapping.Content = kept

	filteredYAML, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, false
	}

	var buf bytes.Buffer
	buf.Write(openSep)
	buf.Write(filteredYAML)
	buf.Write(closeSep)
	buf.Write(body)
	return buf.Bytes(), true
}

// memFile is a read-only fs.File over rewritten SKILL.md bytes; ADK never Stats it, so Stat is minimal.
type memFile struct {
	*bytes.Reader
	size int64
}

func (m *memFile) Close() error               { return nil }
func (m *memFile) Stat() (fs.FileInfo, error) { return memFileInfo{m.size}, nil }

type memFileInfo struct{ size int64 }

func (i memFileInfo) Name() string       { return "SKILL.md" }
func (i memFileInfo) Size() int64        { return i.size }
func (i memFileInfo) Mode() fs.FileMode  { return 0o444 }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return false }
func (i memFileInfo) Sys() any           { return nil }
