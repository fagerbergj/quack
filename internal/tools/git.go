package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

const (
	maxGitOutputBytes = 64 * 1024
	gitTruncMarker    = "\n... (truncated)"
)

// Env for git child processes only (never on quack server).
const (
	GitAskpassTokenEnv = "QUACK_GIT_ASKPASS_TOKEN"
	GitAskpassUserEnv  = "QUACK_GIT_ASKPASS_USERNAME"
	GitAskpassHostEnv  = "QUACK_GIT_ASKPASS_HOST"
)

// GitAskpassLinkName: symlink path for GIT_ASKPASS.
const GitAskpassLinkName = ".quack-askpass"

// Answers git's two-call credential protocol, only for a prompt naming the expected host.
func GitAskpassAnswer(prompt string) string {
	if !askpassHostMatches(prompt, os.Getenv(GitAskpassHostEnv)) {
		return ""
	}
	if strings.Contains(strings.ToLower(prompt), "username") {
		return os.Getenv(GitAskpassUserEnv)
	}
	return os.Getenv(GitAskpassTokenEnv)
}

// askpassHostMatches: git prompts "Username for 'https://host': " / "Password for 'https://user@host': ".
func askpassHostMatches(prompt, want string) bool {
	start, end := strings.Index(prompt, "'"), strings.LastIndex(prompt, "'")
	if want == "" || start < 0 || end <= start {
		return false
	}
	u, err := url.Parse(prompt[start+1 : end])
	return err == nil && asciiEqualFold(u.Host, want)
}

// asciiEqualFold folds A-Z only: strings.EqualFold would match "\u212A" (Kelvin) to "k", a different DNS name.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ensureAskpassLink: ensures .quack-askpass symlink to current binary; tolerates concurrent creation.
func ensureAskpassLink(root string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("git: resolve own binary path for GIT_ASKPASS: %w", err)
	}
	link := filepath.Join(root, GitAskpassLinkName)
	if dest, err := os.Readlink(link); err == nil && dest == self {
		return link, nil
	}
	_ = os.Remove(link)
	if err := os.Symlink(self, link); err != nil {
		// Concurrent creator may have won.
		if dest, rerr := os.Readlink(link); rerr == nil && dest == self {
			return link, nil
		}
		return "", fmt.Errorf("git: create askpass symlink %q: %w", link, err)
	}
	return link, nil
}

// gitAuth: resolved credential for one git child process.
type gitAuth struct {
	cred    GitCredential
	askpass string
	host    string // askpass answers only prompts for this host
}

// GitTokenSource: dynamic per-host credential source.
type GitTokenSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// authFor: resolves credential for rawURL's host (static first, then GitTokenSource).
func (b gitBinding) authFor(rawURL string) (*gitAuth, error) {
	cred := b.credentialFor(rawURL)
	if cred == nil && b.tokenSource != nil {
		c, err := b.tokenSource.GitCredential(context.Background(), rawURL)
		if err != nil {
			return nil, err
		}
		cred = c
	}
	if cred == nil {
		return nil, nil
	}
	link, err := ensureAskpassLink(b.jail.Root())
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("git: parse %q: %w", rawURL, err)
	}
	return &gitAuth{cred: *cred, askpass: link, host: u.Host}, nil
}

type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// gitBinding: userID, jail, caps, credentials - closed over at construction.
type gitBinding struct {
	userID      string
	jail        *workspace.Jail
	caps        workspace.Caps
	credentials []GitCredential
	tokenSource GitTokenSource
	cwd         string
	chatID      string
	nodeDir     string
}

// resolve: cwd-, node- and chat-aware Jail.Resolve for git tool paths.
func (b gitBinding) resolve(p string) (string, error) {
	return b.jail.Resolve(b.userID, b.chatID, jailPath(b.nodeDir, b.cwd, p))
}

// resolveRepoDir: resolve for a dir quack clones or adds a worktree into (see workspace.Jail.ResolveRepoDir).
func (b gitBinding) resolveRepoDir(p string) (string, error) {
	return b.jail.ResolveRepoDir(b.userID, b.chatID, jailPath(b.nodeDir, b.cwd, p))
}

func (b gitBinding) credentialFor(rawURL string) *GitCredential {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	host := u.Hostname()
	for i := range b.credentials {
		if asciiEqualFold(b.credentials[i].Host, host) {
			return &b.credentials[i]
		}
	}
	return nil
}

func gitBinaryPath() (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git: the git binary is not installed in this image: %w", err)
	}
	return p, nil
}

// gitChildPath: the scrubbed PATH for a git child.
func gitChildPath(caps workspace.Caps) string {
	base := "/usr/bin:/bin"
	if len(caps.ExtraPath) == 0 {
		return base
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + base
}

// gitEnv: HOME and the config-source pins come from workspace.GitCmd, after these.
func gitEnv(caps workspace.Caps, auth *gitAuth) []string {
	env := []string{
		"PATH=" + gitChildPath(caps),
		// Only unconfined git's scratch files land here (objects are staged inside .git); confined git gets
		// GitCmd's per-call HOME instead.
		"TMPDIR=" + workspace.SandboxTmpDir(caps),
	}
	// workspace.env for hooks/filters to find the toolchain.
	for _, k := range slices.Sorted(maps.Keys(caps.Env)) {
		if keepForGit(k) {
			env = append(env, k+"="+caps.Env[k])
		}
	}
	if auth != nil {
		env = append(env,
			"GIT_ASKPASS="+auth.askpass,
			GitAskpassUserEnv+"="+auth.cred.Username,
			GitAskpassTokenEnv+"="+auth.cred.Token,
			GitAskpassHostEnv+"="+auth.host,
		)
	}
	return env
}

// keepForGit drops workspace.env keys that would redirect quack's own git (GIT_DIR, GIT_CONFIG_*, GIT_EXEC_PATH,
// SSH_ASKPASS); a private CA bundle still passes.
func keepForGit(k string) bool {
	if k == "GIT_SSL_CAINFO" || k == "GIT_SSL_CAPATH" {
		return true
	}
	return !strings.HasPrefix(k, "GIT_") && k != "SSH_ASKPASS"
}

// runGit runs in dir as its own quack-created clone ("" for no repo).
func runGit(ctx context.Context, dir string, argv []string, caps workspace.Caps, auth *gitAuth) (stdout, stderr string, err error) {
	return runGitIn(ctx, dir, dir, argv, caps, auth)
}

// runGitIn runs in dir, which must be clone or a linked worktree of it; rw as workspace.GitCmd's.
func runGitIn(ctx context.Context, clone, dir string, argv []string, caps workspace.Caps, auth *gitAuth, rw ...string) (stdout, stderr string, err error) {
	bin, err := gitBinaryPath()
	if err != nil {
		return "", "", err
	}
	timeout := caps.Timeout
	if timeout <= 0 {
		timeout = workspace.DefaultCaps().Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, done, err := workspace.GitCmd(cctx, bin, clone, dir, argv, gitEnv(caps, auth), rw...)
	if err != nil {
		return "", "", err
	}
	defer done()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	out := clip(outBuf.String(), maxGitOutputBytes, gitTruncMarker)
	errOut := clip(errBuf.String(), maxGitOutputBytes, gitTruncMarker)

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	slog.Info("git", "component", "tools", "argv", strings.Join(argv, " "), "dir", dir, "exit", exitCode)

	if cctx.Err() == context.DeadlineExceeded {
		return out, errOut, fmt.Errorf("git %s: timed out after %s", strings.Join(argv, " "), timeout)
	}
	if runErr != nil {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = runErr.Error()
		}
		return out, errOut, fmt.Errorf("git %s: %s", strings.Join(argv, " "), msg)
	}
	return out, errOut, nil
}

// validateCloneURL: enforces https-only, rejects URLs with inline credentials.
func validateCloneURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("git_clone: invalid url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("git_clone: only https:// URLs are allowed (got %q)", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("git_clone: credentials in the URL are rejected; configure workspace.git_credentials for private repos instead")
	}
	return u, nil
}

func (b gitBinding) cloneRepo(rawURL, dir, branch string) error {
	if err := validateRef(branch, "git_clone"); branch != "" && err != nil {
		return err
	}
	target, err := b.resolve(dir)
	if err != nil {
		return err
	}
	relRoot, err := b.resolve("")
	if err != nil {
		return err
	}
	// Jail may not exist yet.
	if err := os.MkdirAll(relRoot, 0o755); err != nil {
		return fmt.Errorf("git_clone: create workspace dir: %w", err)
	}
	argv := []string{"clone", "--quiet", "--depth", "1"}
	if branch != "" {
		argv = append(argv, "--branch", branch)
	}
	argv = append(argv, rawURL, target)

	auth, err := b.authFor(rawURL)
	if err != nil {
		return err
	}
	// Run from an empty dir: a repo enclosing relRoot must not lend clone its config.
	_, _, err = runGitIn(context.Background(), "", "", argv, b.caps, auth, target)
	return err
}

// validateRef: rejects refs starting with "-" (flag smuggling).
func validateRef(ref, tool string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("%s: ref must not be empty", tool)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("%s: ref %q looks like a command-line option, not a ref", tool, ref)
	}
	return nil
}

// System identity for all quack commits. Exported for quack-extensions/github's own-commit detection.
const (
	GitCommitAuthorName  = "quack"
	GitCommitAuthorEmail = "agent@quack.local"
)
