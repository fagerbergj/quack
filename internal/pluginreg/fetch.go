package pluginreg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// gitTimeout bounds every git call: a hung remote must not block a fetch or
// an update check.
const gitTimeout = 60 * time.Second

// A full 40-hex sha is pinned and never behind; a short prefix like
// "deadbeef" may be a branch name, so it is resolved, not compared.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// cloneLocks serializes concurrent Fetch calls on the same clone dir, so two
// in-flight fetches of one plugin can't RemoveAll each other's clone.
var cloneLocks sync.Map // map[string]*sync.Mutex

func lockClone(dir string) func() {
	v, _ := cloneLocks.LoadOrStore(dir, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// RemoteURL is overridable so tests can fetch from a local bare repo fixture
// instead of github.com.
var RemoteURL = func(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// Fetch clones or updates p's clone to its target ref and returns the
// updated row. Free function (not an FSRegistry method) so P3's sqlite/
// postgres backends can reuse the same git logic against their own root.
func Fetch(ctx context.Context, root string, p Plugin) (Plugin, error) {
	return fetch(ctx, root, p, nil)
}

// fetch runs commit (when set) on the row it is about to check out, so a
// failed commit leaves the row and the tree both on the old sha.
func fetch(ctx context.Context, root string, p Plugin, commit func(Plugin) error) (Plugin, error) {
	if p.Source == SourceLocal {
		return p, nil // no clone; Root() serves the path directly
	}
	dir := CloneDir(root, p.Name)
	defer lockClone(dir)()
	url := RemoteURL(p.Owner, p.Repo)
	if err := fetchInto(ctx, dir, url); err != nil {
		p.Error = err.Error()
		return p, err
	}
	sha, err := resolveSHA(ctx, dir, p.Ref)
	if err != nil {
		p.Error = err.Error()
		return p, err
	}
	next := p
	now := time.Now().UTC()
	next.SHA, next.FetchedAt, next.Error = sha, &now, ""
	if commit != nil {
		if err := commit(next); err != nil {
			p.Error = err.Error()
			return p, err
		}
	}
	// sha comes from rev-parse --verify, so it's safe bare ("--" would make it a
	// pathspec). In place: processes started from the old checkout see new files.
	if err := gitRun(ctx, dir, "checkout", "--quiet", "--no-guess", "--detach", sha); err != nil {
		p.Error = fmt.Sprintf("checkout: %v", err) // HEAD didn't move; fetchAndPut's compensating Put restores the old sha
		return p, err
	}
	return next, nil
}

// fetchAndPut persists the new sha BEFORE checking it out, so a row never
// names an older sha than its tree (MCP reuse keys on it); a failure Puts the old sha with its error.
func fetchAndPut(ctx context.Context, root string, reg Registry, p Plugin) (Plugin, error) {
	p, fetchErr := fetch(ctx, root, p, func(next Plugin) error { return reg.Put(ctx, next) })
	if fetchErr == nil {
		return p, nil
	}
	return p, errors.Join(fetchErr, reg.Put(ctx, p))
}

// Fetch runs the free Fetch against r's root and persists the result
// regardless of success, so a failure is still visible via List.
func (r *FSRegistry) Fetch(ctx context.Context, p Plugin) (Plugin, error) {
	return fetchAndPut(ctx, r.root, r, p)
}

// fetchInto clones dir if absent or not a git repo, else points origin at
// url (in case the entry moved to a different repo) and fetches all
// branches and tags, --force so a moved tag's local ref follows the remote.
func fetchInto(ctx context.Context, dir, url string) error {
	if isGitRepo(ctx, dir) {
		match, err := originMatches(ctx, dir, url)
		if err != nil {
			return err
		}
		if !match {
			if err := gitRun(ctx, dir, "remote", "set-url", "origin", url); err != nil {
				return err
			}
		}
		return gitRun(ctx, dir, "fetch", "--quiet", "--force", "--prune", "--tags", "origin", "+refs/heads/*:refs/remotes/origin/*")
	}
	if _, err := os.Stat(dir); err == nil {
		// A dir with no working .git (e.g. a killed clone) - reclone once
		// rather than wedge the plugin forever on a corrupt tree.
		if rerr := workspace.RepoRedirect(dir, dir); rerr != nil {
			slog.Warn("pluginreg: discarding a clone that failed quack's repository checks; recloning", "component", "pluginreg", "dir", dir, "err", rerr)
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return cloneInto(ctx, dir, url)
}

// cloneInto clones url to dir via dir's real parent, since the confined clone grant refuses any symlink on its path
// (the plugins root is the operator's). Full, not blob-filtered: the pi-acp shim and skilltoolset read files.
func cloneInto(ctx context.Context, dir, url string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	_, err = runGitRW(ctx, "", []string{abs}, "clone", "--quiet", url, abs)
	return err
}

// isGitRepo reports whether dir is itself a git repo, not merely inside one -
// rev-parse --git-dir walks up to an ancestor repo otherwise, which would
// mistake a killed clone nested in the user's own checkout for that repo.
func isGitRepo(ctx context.Context, dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	out, err := gitOutput(ctx, dir, "rev-parse", "--git-dir")
	if err != nil {
		return false
	}
	gitDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	rel, err := filepath.Rel(dir, filepath.Clean(gitDir))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func originMatches(ctx context.Context, dir, want string) (bool, error) {
	out, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == want, nil
}

// resolveSHA: "" follows the remote default branch (set-head --auto first,
// the local HEAD symref never updates otherwise); a pinned ref is tried as
// remote branch, then tag, then bare commit-ish.
func resolveSHA(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		if err := gitRun(ctx, dir, "remote", "set-head", "origin", "--auto"); err != nil {
			return "", fmt.Errorf("resolve default branch: %w", err)
		}
		return gitVerify(ctx, dir, "refs/remotes/origin/HEAD^{commit}")
	}
	candidates := []string{
		"refs/remotes/origin/" + ref + "^{commit}",
		"refs/tags/" + ref + "^{commit}",
		ref + "^{commit}",
	}
	for _, c := range candidates {
		if sha, err := gitVerify(ctx, dir, c); err == nil {
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref %q not found in the clone", ref)
}

func gitVerify(ctx context.Context, dir, rev string) (string, error) {
	out, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// CheckUpdate compares the remote sha for p's tracked ref against its
// installed sha. A pinned sha (Ref is a full commit) is never behind.
func CheckUpdate(ctx context.Context, p Plugin) (behind bool, remoteSHA string, err error) {
	if p.Source == SourceLocal {
		return false, "", nil
	}
	if p.Ref != "" && shaPattern.MatchString(p.Ref) {
		return false, p.Ref, nil
	}
	url := RemoteURL(p.Owner, p.Repo)
	if p.Ref == "" {
		remoteSHA, err = lsRemoteHEAD(ctx, url)
	} else {
		remoteSHA, err = lsRemoteRef(ctx, url, p.Ref)
	}
	if err != nil {
		return false, "", err
	}
	return remoteSHA != "" && remoteSHA != p.SHA, remoteSHA, nil
}

func (r *FSRegistry) CheckUpdate(ctx context.Context, p Plugin) (bool, string, error) {
	return CheckUpdate(ctx, p)
}

// lsRemoteHEAD returns the sha the remote's default branch currently points at.
func lsRemoteHEAD(ctx context.Context, url string) (string, error) {
	out, err := gitOutput(ctx, "", "ls-remote", url, "HEAD")
	if err != nil {
		return "", err
	}
	return firstField(out), nil
}

// lsRemoteRef resolves a branch/tag name to its sha, branch then peeled tag
// then tag - the same precedence resolveSHA uses locally, so CheckUpdate and
// Fetch never disagree on a same-named branch+tag pair.
func lsRemoteRef(ctx context.Context, url, ref string) (string, error) {
	out, err := gitOutput(ctx, "", "ls-remote", url, "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	if err != nil {
		return "", err
	}
	var head, tag, peeledTag string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, refName := fields[0], fields[1]
		switch refName {
		case "refs/heads/" + ref:
			head = sha
		case "refs/tags/" + ref + "^{}":
			peeledTag = sha
		case "refs/tags/" + ref:
			tag = sha
		}
	}
	for _, sha := range []string{head, peeledTag, tag} {
		if sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref %q not found on remote", ref)
}

func firstField(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// gitPassEnv is all registry git inherits from the server: the route to the remote (proxy, private CA) and scratch space.
var gitPassEnv = []string{
	"PATH", "TMPDIR", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
	"ALL_PROXY", "all_proxy", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// gitEnv: GITHUB_TOKEN rides GIT_CONFIG_* (not argv, which failing stderr can echo onto the row), keyed to
// https://github.com/, which git matches against the requested URL; a cross-host redirect is not covered.
func gitEnv() []string {
	var env []string
	for _, k := range gitPassEnv {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: bearer "+token,
		)
	}
	return env
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := runGit(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return runGit(ctx, dir, args...)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitRW(ctx, dir, nil, args...)
}

// runGitRW runs git in dir; rw as workspace.GitCmd's.
func runGitRW(ctx context.Context, dir string, rw []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd, done, err := workspace.GitCmd(ctx, "git", dir, dir, args, gitEnv(), rw...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	defer done()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Only the subcommand name, never the full args: they can carry a
		// url or ref an operator pasted from somewhere sensitive, and this
		// message is persisted verbatim onto the row (entry.json "error").
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
