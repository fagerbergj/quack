// Package pluginreg is the dynamic plugin registry (epic #1427, P0 #1428):
// entries parsed from quack.yaml's plugins.seed, a filesystem-backed store of
// resolved rows, and git fetch/update-check against the entry's remote.
package pluginreg

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// SourceGitHub is a `github:owner/repo[@ref][#path]` entry, cloned under
	// the registry root.
	SourceGitHub = "github"
	// SourceLocal is any entry not starting with "github:" - today's bare
	// plugins: list, kept working unchanged. No clone: Root is the path itself.
	SourceLocal = "local"
)

// entryPattern is github:owner/repo[@ref][#path]. Groups: owner, repo, ref,
// path. \s is excluded from every group so no field can carry whitespace.
var entryPattern = regexp.MustCompile(`^github:([^/@#\s]+)/([^/@#\s]+)(?:@([^#\s]+))?(?:#([^\s]+))?$`)

// Entry is one parsed plugins.seed line.
type Entry struct {
	Raw    string
	Source string // SourceGitHub or SourceLocal

	// GitHub fields (Source == SourceGitHub).
	Owner string
	Repo  string
	Ref   string // "" = untracked, follows the default branch
	Path  string // "" = plugin root is the repo root; always cleaned, relative, non-escaping

	// Root is set only for a local entry: the path itself, verbatim.
	Root string
}

// Name is the plugin's registry-row name: the repo name for a github entry
// (plugin.json may override it in P1), or the last path element for a local
// one - the raw path itself isn't usable as a registry directory name, since
// it typically contains separators (e.g. ".agents/vendor/dotagents").
func (e Entry) Name() string {
	if e.Source == SourceGitHub {
		return e.Repo
	}
	return filepath.Base(e.Raw)
}

// ParseEntry parses one plugins.seed line. `github:owner/repo[@ref][#path]`
// is a github entry; anything else is a local-root entry (today's bare
// plugins: list form), with no clone and Root equal to the string itself.
func ParseEntry(s string) (Entry, error) {
	if s == "" {
		return Entry{}, fmt.Errorf("plugin entry is empty")
	}
	if len(s) < 7 || s[:7] != "github:" {
		return Entry{Raw: s, Source: SourceLocal, Root: s}, nil
	}

	m := entryPattern.FindStringSubmatch(s)
	if m == nil {
		return Entry{}, fmt.Errorf("plugin entry %q does not match github:owner/repo[@ref][#path]", s)
	}
	owner, repo, ref, path := m[1], m[2], m[3], m[4]
	repo = strings.TrimSuffix(repo, ".git")
	if err := validName(owner); err != nil {
		return Entry{}, fmt.Errorf("plugin entry %q: owner %v", s, err)
	}
	if err := validName(repo); err != nil {
		return Entry{}, fmt.Errorf("plugin entry %q: repo %v", s, err)
	}
	// A ref starting with "-" would be read as a git flag wherever it is
	// interpolated into a bare rev-parse/checkout argument.
	if ref != "" && ref[0] == '-' {
		return Entry{}, fmt.Errorf("plugin entry %q: ref %q must not start with \"-\"", s, ref)
	}
	cleanPath, err := cleanSubPath(path)
	if err != nil {
		return Entry{}, fmt.Errorf("plugin entry %q: %v", s, err)
	}
	return Entry{Raw: s, Source: SourceGitHub, Owner: owner, Repo: repo, Ref: ref, Path: cleanPath}, nil
}

// cleanSubPath validates and normalizes #path: relative, and never escaping
// the plugin root once joined - checked again at Plugin.Root() since a row
// read back from disk is trusted without re-parsing.
func cleanSubPath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path %q must be relative", raw)
	}
	clean := filepath.Clean(raw)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the plugin root", raw)
	}
	return clean, nil
}

// validName rejects a repo/owner/registry-row name that is empty, ".", "..",
// or contains a path separator - anything else becomes a directory name
// under the registry root (FSRegistry.Put/Delete), so this is also the
// traversal guard for those.
func validName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid name %q", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid name %q: contains a path separator", name)
	}
	return nil
}
