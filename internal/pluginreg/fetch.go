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

// fetch runs commit (when set) on the row it is about to check out, so a
// failed commit leaves the row and the tree both on the old sha.
func fetch(ctx context.Context, root string, p Plugin, commit func(Plugin) error) (Plugin, error) {
	if p.Source == SourceLocal {
		return p, nil // no clone; Root() serves the path directly
	}
	dir, err := realParentDir(CloneDir(root, p.Name))
	if err != nil {
		p.Error = err.Error()
		return p, err
	}
	defer lockClone(dir)()
	url := RemoteURL(p.Owner, p.Repo)
	if err := fetchInto(ctx, dir, url, p.Ref); err != nil {
		p.Error = err.Error()
		return p, err
	}
	sha, err := gitVerify(ctx, dir, fetchedRef+"^{commit}")
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

// Fetch clones or updates p under r's root and persists the result
// regardless of success, so a failure is still visible via List.
func (r *FSRegistry) Fetch(ctx context.Context, p Plugin) (Plugin, error) {
	return fetchAndPut(ctx, r.root, r, p)
}

// fetchedRef holds the one commit the last fetch brought in for the row's ref.
const fetchedRef = "refs/quack/fetched"

// fetchInto force-fetches only ref's commit, depth 1, into fetchedRef, so a moved tag follows the remote and a row
// of a large monorepo never pulls its history. Not blob-filtered: the pi-acp shim and skilltoolset read files.
func fetchInto(ctx context.Context, dir, url, ref string) error {
	if err := ensureRepo(ctx, dir, url); err != nil {
		return err
	}
	src, err := remoteSource(ctx, url, ref)
	if err != nil {
		return err
	}
	return gitRun(ctx, dir, "fetch", "--quiet", "--force", "--no-tags", "--depth=1", "origin", "+"+src+":"+fetchedRef)
}

// ensureRepo makes dir a repo with origin at url: kept and repointed if it is one (the entry may have moved),
// else created empty.
func ensureRepo(ctx context.Context, dir, url string) error {
	if isGitRepo(ctx, dir) {
		match, err := originMatches(ctx, dir, url)
		if err != nil || match {
			return err
		}
		return gitRun(ctx, dir, "remote", "set-url", "origin", url)
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
	if _, err := runGitRW(ctx, "", []string{dir}, "init", "--quiet", dir); err != nil {
		return err
	}
	return gitRun(ctx, dir, "remote", "add", "origin", url)
}

// remoteSource is what to fetch for ref: the default branch, a full sha as-is, else the branch or tag
// ls-remote finds, branch first as CheckUpdate resolves it.
func remoteSource(ctx context.Context, url, ref string) (string, error) {
	if ref == "" {
		return "HEAD", nil
	}
	if shaPattern.MatchString(ref) {
		return ref, nil
	}
	_, name, err := lsRemoteRef(ctx, url, ref)
	return name, err
}

// realParentDir is dir under its parent's real path, the parent created: quack's git refuses a repo path through a
// symlink, and the operator's plugins root may be one.
func realParentDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// isGitRepo reports whether dir is itself a repo: rev-parse --git-dir walks up, mistaking a killed clone
// nested in the user's checkout for that checkout.
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
		remoteSHA, _, err = lsRemoteRef(ctx, url, p.Ref)
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

// lsRemoteRef resolves branch, then peeled tag, then tag, returning the sha and the ref to fetch, so CheckUpdate
// and Fetch agree on a same-named branch+tag pair.
func lsRemoteRef(ctx context.Context, url, ref string) (sha, name string, err error) {
	out, err := gitOutput(ctx, "", "ls-remote", url, "refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	if err != nil {
		return "", "", err
	}
	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			found[fields[1]] = fields[0]
		}
	}
	head, tag := "refs/heads/"+ref, "refs/tags/"+ref
	for _, c := range []struct{ listed, fetch string }{{head, head}, {tag + "^{}", tag}, {tag, tag}} {
		if sha := found[c.listed]; sha != "" {
			return sha, c.fetch, nil
		}
	}
	return "", "", fmt.Errorf("ref %q is not a branch or tag on the remote (pin a commit by its full sha)", ref)
}

func firstField(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// gitPassEnv is all registry git inherits from the server: the route to the remote (proxy, private CA)
// and scratch space.
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
		// Only the subcommand name: args can carry a sensitive url or ref, and this message is persisted on the row.
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
