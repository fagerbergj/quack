package workspace

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// git_clone puts repo at depth 1; one extra level for nesting.
const repoSearchDepth = 2

// FindRepos returns git repos at root or beneath, to repoSearchDepth. Skips vendored/ignored dirs.
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

func skipDir(name string) bool { return SkipDir(name) }

// Reports whether a directory is vendored/generated (results can be harmful: minified bundles can be MB per match).
func SkipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "__pycache__":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// RepoKey: the chat's shared memory bucket key, keyed by origin identity not
// repo count - worktree-per-node means a clone plus N linked worktrees of the
// SAME origin all resolve to one bucket. "" when no repo, no origin, or the
// found repos genuinely disagree (don't guess).
func (j *Jail) RepoKey(userID, chatID string) string {
	root, err := j.Resolve(userID, chatID, "")
	if err != nil {
		return ""
	}
	repos := FindRepos(root)
	key := ""
	for _, r := range repos {
		id := RepoIdentity(r)
		if id == "" {
			continue
		}
		if key == "" {
			key = id
		} else if key != id {
			slog.Warn("workspace: chat scope has repos with disagreeing origins, memory repo bucket disabled",
				"component", "workspace", "chat_id", chatID)
			return ""
		}
	}
	return key
}

// Stable, clone-URL-independent identity from .git/config origin remote. Parsed from file (no git subprocess, recall hot path).
func RepoIdentity(dir string) string {
	configPath := filepath.Join(dir, ".git", "config")
	// A linked worktree's .git is a pointer file; origin lives in the parent
	// clone's config, reached via the commondir it names.
	if _, common := worktreeGitDirs(dir); common != "" {
		configPath = filepath.Join(common, "config")
	}
	f, err := os.Open(configPath)
	if err != nil {
		return ""
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
				return NormalizeRepoURL(strings.TrimSpace(v))
			}
		}
	}
	return ""
}

// NormalizeRepoURL collapses git@/https:///ssh:// forms to one key:
// "github.com/owner/repo". Exported so callers with a raw clone URL (e.g. a
// dispatch's dag.Setup.Repo) but no cloned worktree can derive the same
// memory.Scope.Repo key RepoIdentity computes from an actual clone's origin.
func NormalizeRepoURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.Replace(u, ":", "/", 1)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}
