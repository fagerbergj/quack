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
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			keys[name] = true
		}
	}
	return keys
}()

// NewFileSystemSource wraps fsys in the frontmatter filter before handing it
// to ADK's skill.NewFileSystemSource, so a SKILL.md carrying a field ADK's
// KnownFields(true) decoder doesn't recognize (e.g. Claude Code's
// argument-hint, #1084) still loads instead of being skipped by Tolerant.
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
	defer file.Close()

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

// lfSep/crlfSep mirror ADK's own two accepted separator forms
// (frontmatter.go's frontmatterSeparator/frontmatterSeparatorWin); a BOM
// prefix is out of scope, ADK fails on that regardless of this wrapper.
var lfSep = []byte("---\n")
var crlfSep = []byte("---\r\n")

// leadingSep returns whichever separator form content starts with.
func leadingSep(content []byte) (sep []byte, ok bool) {
	if bytes.HasPrefix(content, crlfSep) {
		return crlfSep, true
	}
	if bytes.HasPrefix(content, lfSep) {
		return lfSep, true
	}
	return nil, false
}

// findSep returns the earliest occurrence of either separator form in b.
func findSep(b []byte) (idx, seplen int, ok bool) {
	iLF := bytes.Index(b, lfSep)
	iCRLF := bytes.Index(b, crlfSep)
	switch {
	case iLF == -1 && iCRLF == -1:
		return 0, 0, false
	case iCRLF != -1 && (iLF == -1 || iCRLF <= iLF):
		return iCRLF, len(crlfSep), true
	default:
		return iLF, len(lfSep), true
	}
}

// filterFrontmatter drops unknown top-level keys from the YAML frontmatter
// block of a SKILL.md's bytes, leaving the "---" delimiters and markdown body
// byte-identical. It edits the YAML AST (yaml.Node) rather than decoding
// through a generic map: a map round-trip re-serializes each scalar from its
// resolved Go type, so `007` comes back `7` and `1.0` comes back `1` even
// with no unknown key present - a silent corruption of every skill, not just
// the ones this wrapper exists for. Node-level editing keeps each surviving
// key's original style/tag, so its literal text is untouched.
//
// If no key is unknown, content is returned as-is: no parse-and-rewrite for
// the common case, which matters because ListFrontmatters runs on every
// skill on every agent turn (SkillToolset.ProcessRequest), not just at
// startup.
//
// ok is false when the content isn't valid ADK-shaped frontmatter (no
// separators, bad YAML) - the caller passes the original bytes through
// unchanged so Tolerant's own parse still decides skip-vs-fatal.
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

// memFile is a read-only fs.File over rewritten SKILL.md bytes. ADK only
// reads and closes the frontmatter file, never Stats it, so a minimal Stat
// is enough to satisfy the fs.File interface.
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
