package pluginreg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// gitTimeout bounds every git call: a hung remote must not block a fetch or
// an update check (mirrors internal/plugin/refresh.go's refreshTimeout).
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

// remoteURL is overridable so tests can fetch from a local bare repo fixture
// instead of github.com.
var remoteURL = func(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// Fetch clones or updates p's clone to its target ref and returns the
// updated row. Free function (not an FSRegistry method) so P3's sqlite/
// postgres backends can reuse the same git logic against their own root.
func Fetch(ctx context.Context, root string, p Plugin) (Plugin, error) {
	if p.Source == SourceLocal {
		return p, nil // no clone; Root() serves the path directly
	}
	dir := CloneDir(root, p.Name)
	defer lockClone(dir)()
	url := remoteURL(p.Owner, p.Repo)
	if err := fetchInto(ctx, dir, url); err != nil {
		p.Error = err.Error()
		return p, err
	}
	sha, err := resolveSHA(ctx, dir, p.Ref)
	if err != nil {
		p.Error = err.Error()
		return p, err
	}
	// sha is resolved via rev-parse --verify, never a raw ref, so it's safe
	// bare here - "--" would make checkout read it as a pathspec instead.
	if err := gitRun(ctx, dir, "checkout", "--quiet", "--no-guess", "--detach", sha); err != nil {
		p.Error = fmt.Sprintf("checkout: %v", err)
		return p, err
	}
	now := time.Now().UTC()
	p.SHA = sha
	p.FetchedAt = &now
	p.Error = ""
	return p, nil
}

// Fetch runs the free Fetch against r's root and persists the result
// regardless of success, so a failure is still visible via List.
func (r *FSRegistry) Fetch(ctx context.Context, p Plugin) (Plugin, error) {
	p, fetchErr := Fetch(ctx, r.root, p)
	putErr := r.Put(ctx, p)
	return p, errors.Join(fetchErr, putErr)
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
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	// Full clone, not blob-filtered: the pi-acp shim and skilltoolset read
	// files, and replay needs git history.
	return gitRun(ctx, "", "clone", "--quiet", url, dir)
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
	url := remoteURL(p.Owner, p.Repo)
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

// gitAuthEnv carries GITHUB_TOKEN as an http.extraheader via git's env-based
// config (GIT_CONFIG_*) rather than argv, so a failing command's stderr -
// which Fetch stores verbatim on the row - never contains the token.
func gitAuthEnv() []string {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: bearer " + token,
	}
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := runGit(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return runGit(ctx, dir, args...)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env := gitAuthEnv(); env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
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
