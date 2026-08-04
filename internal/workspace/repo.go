package workspace

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// repoSearchDepth bounds the walk below the starting directory. git_clone puts a
// repo at <scope>/<dir> (depth 1); one extra level covers a worker that nested it.
const repoSearchDepth = 2

// FindRepos returns the git repositories at root or beneath it, to
// repoSearchDepth. Vendored/ignored trees (node_modules, vendor, and dot-dirs -
// notably .git itself) are skipped: a dependency carrying its own .git must not
// masquerade as the node's repo, nor make the real one look ambiguous. Once a
// directory IS a repo, the walk stops there (a submodule is not a second repo).
func FindRepos(root string) []string {
	if isRepo(root) {
		return []string{root}
	}
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > repoSearchDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || skipDir(e.Name()) {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if isRepo(sub) {
				out = append(out, sub)
				continue // don't descend into a repo - submodules aren't candidates
			}
			walk(sub, depth+1)
		}
	}
	walk(root, 1)
	return out
}

// skipDir names the directories a repo search must never descend into.
func skipDir(name string) bool { return SkipDir(name) }

// skipDirNames are the fixed vendored/generated directory names SkipDir
// matches exactly (dot-dirs are handled separately, by prefix). The single
// source of truth for both SkipDir and SkipGlobs, so the two can't drift.
var skipDirNames = []string{"node_modules", "vendor", "target", "dist", "build", "__pycache__"}

// SkipDir reports whether a directory is vendored or generated - a tree no search
// should descend into. It covers dot-dirs (.git, .next, .venv), dependency trees
// (node_modules, vendor) and the conventional build outputs.
//
// Searching these is never what anyone means, and the results are actively harmful:
// a live `grep` that matched inside a Next.js build dir returned a 48 MB result (the
// hits were minified .js.map lines), which blew the model's context window and 400'd
// the node. Every other harness gets this for free by shelling out to ripgrep, which
// honours .gitignore.
func SkipDir(name string) bool {
	for _, n := range skipDirNames {
		if name == n {
			return true
		}
	}
	return strings.HasPrefix(name, ".")
}

// SkipGlobs renders SkipDir's boundary as ast-grep/ripgrep-style `!**/name/**`
// exclude globs, for a scanner that walks paths itself via a tool's own
// --globs flag rather than pruning directories entry-by-entry (see the
// package-declaration check, vetting/packagecheck.go) - same tree, one
// definition.
func SkipGlobs() []string {
	globs := make([]string, 0, len(skipDirNames)+1)
	globs = append(globs, "!**/.*/**")
	for _, n := range skipDirNames {
		globs = append(globs, "!**/"+n+"/**")
	}
	return globs
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// RepoKey identifies the repository a chat is working in - the key of its shared
// memory bucket (memory.Scope.Repo), so what one coding agent learns about a repo
// is recalled by the next one working in the SAME repo.
//
// It is derived, not guessed: the chat's jail scope (<root>/<user>/<chat>/) is
// searched for git repos and the single one found is identified by its `origin`
// remote ("github.com/owner/repo" - host + path, normalized across ssh/https and a
// .git suffix, so the same repo cloned either way is the same bucket). It returns ""
// - no repo bucket, memories fall back to the role bucket - when there is no repo,
// when there is more than one (ambiguous: don't guess), or when the single repo has
// no origin remote (a local-only repo has no shareable identity).
func (j *Jail) RepoKey(userID, chatID string) string {
	root, err := j.Resolve(userID, chatID, "")
	if err != nil {
		return ""
	}
	repos := FindRepos(root)
	if len(repos) != 1 {
		return ""
	}
	return RepoIdentity(repos[0])
}

// RepoIdentity is a repo's stable, clone-URL-independent identity, read from its
// .git/config `origin` remote: "github.com/owner/repo". Empty when dir is not a repo
// or has no origin. Parsed from the file (no git subprocess): this runs on the recall
// hot path, once per node turn.
func RepoIdentity(dir string) string {
	f, err := os.Open(filepath.Join(dir, ".git", "config"))
	if err != nil {
		return "" // not a repo, or a worktree/submodule .git file - no identity
	}
	defer f.Close()

	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = strings.HasPrefix(line, `[remote "origin"]`)
			continue
		}
		if !inOrigin {
			continue
		}
		if url, ok := strings.CutPrefix(line, "url"); ok {
			if _, v, found := strings.Cut(url, "="); found {
				return normalizeRepoURL(strings.TrimSpace(v))
			}
		}
	}
	return ""
}

// normalizeRepoURL collapses the ways one repo can be addressed -
// git@github.com:owner/repo.git, https://user@github.com/owner/repo.git,
// ssh://git@github.com/owner/repo - to one key: "github.com/owner/repo".
func normalizeRepoURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 { // strip scheme
		u = u[i+3:]
	}
	if i := strings.Index(u, "@"); i >= 0 { // strip userinfo (git@, token@)
		u = u[i+1:]
	}
	u = strings.Replace(u, ":", "/", 1) // scp form host:owner/repo → host/owner/repo
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}
