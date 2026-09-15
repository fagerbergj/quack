package pluginreg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// gitTimeout bounds every git call: a hung remote must not block a fetch or
// an update check (mirrors internal/plugin/refresh.go's refreshTimeout).
const gitTimeout = 60 * time.Second

// shaPattern matches a ref that is already a commit sha (full or abbreviated) -
// a pinned sha is never behind, and never needs a remote lookup to resolve.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// remoteURL is overridable so tests can fetch from a local bare repo fixture
// instead of github.com.
var remoteURL = func(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

func (r *FSRegistry) url(p Plugin) string { return remoteURL(p.Owner, p.Repo) }

// Fetch clones or updates p's clone to its target ref, records the resulting
// sha and clears the row's error on success. On failure it keeps the old sha
// and stores the error on the row instead - a fetch never fails boot. The
// returned Plugin is already persisted via Put.
func (r *FSRegistry) Fetch(ctx context.Context, p Plugin) (Plugin, error) {
	if p.Source == SourceLocal {
		return p, nil // no clone; Root() serves the path directly
	}
	dir := CloneDir(r.root, p.Name)
	if err := r.fetchInto(ctx, dir, r.url(p)); err != nil {
		p.Error = err.Error()
		return p, r.Put(ctx, p)
	}
	target := p.Ref
	if target == "" {
		branch, err := defaultBranch(ctx, dir)
		if err != nil {
			p.Error = err.Error()
			return p, r.Put(ctx, p)
		}
		target = branch
	}
	if err := gitRun(ctx, dir, "checkout", "--quiet", "--detach", target); err != nil {
		p.Error = fmt.Sprintf("checkout %s: %v", target, err)
		return p, r.Put(ctx, p)
	}
	sha, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		p.Error = err.Error()
		return p, r.Put(ctx, p)
	}
	p.SHA = strings.TrimSpace(sha)
	p.FetchedAt = time.Now().UTC()
	p.Error = ""
	return p, r.Put(ctx, p)
}

// fetchInto clones dir if absent, else fetches all branches and tags,
// --force so a moved tag's local ref follows the remote.
func (r *FSRegistry) fetchInto(ctx context.Context, dir, url string) error {
	if _, err := os.Stat(dir); err == nil {
		return gitRun(ctx, dir, "fetch", "--quiet", "--force", "--prune", "--tags", "origin", "+refs/heads/*:refs/remotes/origin/*")
	}
	// Full clone, not blob-filtered: the pi-acp shim and skilltoolset read
	// files, and replay needs git history.
	_, err := runGit(ctx, "", authArgs([]string{"clone", "--quiet", url, dir})...)
	return err
}

// defaultBranch resolves the remote's HEAD branch from the local clone's
// origin/HEAD symref, set by `git clone` - no network round trip needed.
func defaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve default branch: %w", err)
	}
	ref := strings.TrimSpace(out)
	return strings.TrimPrefix(ref, "refs/remotes/"), nil
}

// CheckUpdate compares the remote sha for p's tracked ref against its
// installed sha. A pinned sha (Ref looks like a commit) is never behind.
func (r *FSRegistry) CheckUpdate(ctx context.Context, p Plugin) (behind bool, remoteSHA string, err error) {
	if p.Source == SourceLocal {
		return false, "", nil
	}
	if p.Ref != "" && shaPattern.MatchString(p.Ref) {
		return false, p.Ref, nil
	}
	url := r.url(p)
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

// lsRemoteHEAD returns the sha the remote's default branch currently points at.
func lsRemoteHEAD(ctx context.Context, url string) (string, error) {
	out, err := gitOutput(ctx, "", authArgs([]string{"ls-remote", url, "HEAD"})...)
	if err != nil {
		return "", err
	}
	return firstField(out), nil
}

// lsRemoteRef resolves a branch or tag name to its commit sha, preferring an
// annotated tag's peeled (^{}) entry.
func lsRemoteRef(ctx context.Context, url, ref string) (string, error) {
	out, err := gitOutput(ctx, "", authArgs([]string{"ls-remote", url, "refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"})...)
	if err != nil {
		return "", err
	}
	sha := ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha = fields[0] // last match wins: peeled tag line sorts after the tag line
	}
	if sha == "" {
		return "", fmt.Errorf("ref %q not found on remote", ref)
	}
	return sha, nil
}

func firstField(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// authArgs prepends a GITHUB_TOKEN bearer header, when set, ahead of the git
// subcommand - used for private repos, applied to every network call.
func authArgs(args []string) []string {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return args
	}
	return append([]string{"-c", "http.extraheader=AUTHORIZATION: bearer " + token}, args...)
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
