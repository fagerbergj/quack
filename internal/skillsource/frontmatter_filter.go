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

// filterFrontmatter drops unknown top-level keys from the YAML frontmatter
// block of a SKILL.md's bytes, leaving the "---" delimiters and markdown body
// byte-identical. ok is false when the content isn't valid ADK-shaped
// frontmatter (no separators, bad YAML) - the caller passes the original
// bytes through unchanged so Tolerant's own parse still decides skip-vs-fatal.
func filterFrontmatter(content []byte) (out []byte, ok bool) {
	sep := []byte("---\n")
	if !bytes.HasPrefix(content, sep) {
		return nil, false
	}
	rest := content[len(sep):]
	end := bytes.Index(rest, sep)
	if end == -1 {
		return nil, false
	}
	body := rest[end+len(sep):]

	var m map[string]any
	if err := yaml.Unmarshal(rest[:end], &m); err != nil {
		return nil, false
	}
	for k := range m {
		if !knownFrontmatterKeys[k] {
			delete(m, k)
		}
	}
	filteredYAML, err := yaml.Marshal(m)
	if err != nil {
		return nil, false
	}

	var buf bytes.Buffer
	buf.Write(sep)
	buf.Write(filteredYAML)
	buf.Write(sep)
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

func (i memFileInfo) Name() string      { return "SKILL.md" }
func (i memFileInfo) Size() int64       { return i.size }
func (i memFileInfo) Mode() fs.FileMode { return 0o444 }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool       { return false }
func (i memFileInfo) Sys() any          { return nil }
