// Package pluginreg is the dynamic plugin registry (epic #1427, P0 #1428):
// entries parsed from quack.yaml's plugins.seed, a filesystem-backed store of
// resolved rows, and git fetch/update-check against the entry's remote.
package pluginreg

import (
	"fmt"
	"regexp"
)

const (
	// SourceGitHub is a `github:owner/repo[@ref][#path]` entry, cloned under
	// the registry root.
	SourceGitHub = "github"
	// SourceLocal is any entry not starting with "github:" - today's bare
	// plugins: list, kept working unchanged. No clone: Root is the path itself.
	SourceLocal = "local"
)

// entryPattern is github:owner/repo[@ref][#path]. Groups: owner, repo, ref, path.
var entryPattern = regexp.MustCompile(`^github:([^/@#]+)/([^/@#]+)(?:@([^#]+))?(?:#(.+))?$`)

// Entry is one parsed plugins.seed line.
type Entry struct {
	Raw    string
	Source string // SourceGitHub or SourceLocal

	// GitHub fields (Source == SourceGitHub).
	Owner string
	Repo  string
	Ref   string // "" = untracked, follows the default branch
	Path  string // "" = plugin root is the repo root

	// Root is set only for a local entry: the path itself, verbatim.
	Root string
}

// Name is the plugin's registry-row name: the repo name for a github entry
// (plugin.json may override it in P1), or the entry itself for a local one.
func (e Entry) Name() string {
	if e.Source == SourceGitHub {
		return e.Repo
	}
	return e.Raw
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
	return Entry{Raw: s, Source: SourceGitHub, Owner: owner, Repo: repo, Ref: ref, Path: path}, nil
}
