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
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

const maxGitOutputBytes = 64 * 1024

// defaultCloneDepth is git_clone's default shallow depth when `depth` is unset.
const defaultCloneDepth = 1

// Env for git child processes only (never on quack server).
const (
	GitAskpassTokenEnv = "QUACK_GIT_ASKPASS_TOKEN"
	GitAskpassUserEnv  = "QUACK_GIT_ASKPASS_USERNAME"
)

// GitAskpassLinkName: symlink path for GIT_ASKPASS.
const GitAskpassLinkName = ".quack-askpass"

// Answers git's two-call credential protocol.
func GitAskpassAnswer(prompt string) string {
	if strings.Contains(strings.ToLower(prompt), "username") {
		return os.Getenv(GitAskpassUserEnv)
	}
	return os.Getenv(GitAskpassTokenEnv)
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
	return &gitAuth{cred: *cred, askpass: link}, nil
}

// GitCredential: per-host HTTPS credential.
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
	allowPush   bool
	cwd         string
	chatID      string
	nodeDir     string
}

// resolve: cwd-, node- and chat-aware Jail.Resolve for git tool paths.
func (b gitBinding) resolve(p string) (string, error) {
	return b.jail.Resolve(b.userID, b.chatID, jailPath(b.nodeDir, b.cwd, p))
}

// credentialFor: matches rawURL's host against configured credentials.
func (b gitBinding) credentialFor(rawURL string) *GitCredential {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for i := range b.credentials {
		if strings.EqualFold(b.credentials[i].Host, host) {
			return &b.credentials[i]
		}
	}
	return nil
}

// runGit: every git tool executes through this function.

// gitBinaryPath: resolves git via PATH.
func gitBinaryPath() (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git: the git binary is not installed in this image: %w", err)
	}
	return p, nil
}

// gitChildPath: scrubbed env for git child.
func gitChildPath(caps workspace.Caps) string {
	base := "/usr/bin:/bin"
	if len(caps.ExtraPath) == 0 {
		return base
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + base
}

func gitEnv(dir string, caps workspace.Caps, auth *gitAuth) []string {
	home := caps.HomeDir
	if home == "" {
		home = dir
	}
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + gitChildPath(caps),
		"HOME=" + home,
	}
	// workspace.env for hooks/filters to find the toolchain.
	for _, k := range slices.Sorted(maps.Keys(caps.Env)) {
		env = append(env, k+"="+caps.Env[k])
	}
	if auth != nil {
		env = append(env,
			"GIT_ASKPASS="+auth.askpass,
			GitAskpassUserEnv+"="+auth.cred.Username,
			GitAskpassTokenEnv+"="+auth.cred.Token,
		)
	}
	return env
}

// capOutput: truncates to max bytes with a suffix.
func capOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// runGit: executes git as a subprocess with scrubbed env and capped output.
func runGit(ctx context.Context, dir string, argv []string, caps workspace.Caps, auth *gitAuth) (stdout, stderr string, err error) {
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

	env := gitEnv(dir, caps, auth)
	// Neutralize hooks (agent-written hooks would run unsandboxed).
	fullArgv := append([]string{"-c", "core.hooksPath=/dev/null"}, argv...)
	cmd := exec.CommandContext(cctx, bin, fullArgv...)
	cmd.Dir = dir
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	out := capOutput(outBuf.String(), maxGitOutputBytes)
	errOut := capOutput(errBuf.String(), maxGitOutputBytes)

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

type gitCloneResult struct {
	Dir           string `json:"dir"`
	Head          string `json:"head"`
	DefaultBranch string `json:"default_branch"`
	Cwd           string `json:"cwd"`
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

// cloneRepo: resolve target, build argv, run clone.
func (b gitBinding) cloneRepo(rawURL, dir string, depthArg *int, branch string) (gitCloneResult, error) {
	if err := validateRef(branch, "git_clone"); branch != "" && err != nil {
		return gitCloneResult{}, err
	}
	target, err := b.resolve(dir)
	if err != nil {
		return gitCloneResult{}, err
	}
	relRoot, err := b.resolve("")
	if err != nil {
		return gitCloneResult{}, err
	}
	// Jail may not exist yet.
	if err := os.MkdirAll(relRoot, 0o755); err != nil {
		return gitCloneResult{}, fmt.Errorf("git_clone: create workspace dir: %w", err)
	}

	depth := defaultCloneDepth
	if depthArg != nil {
		depth = *depthArg
	}
	argv := []string{"clone", "--quiet"}
	if depth > 0 {
		argv = append(argv, "--depth", strconv.Itoa(depth))
	}
	if branch != "" {
		argv = append(argv, "--branch", branch)
	}
	argv = append(argv, rawURL, target)

	auth, err := b.authFor(rawURL)
	if err != nil {
		return gitCloneResult{}, err
	}
	if _, _, err := runGit(context.Background(), relRoot, argv, b.caps, auth); err != nil {
		return gitCloneResult{}, err
	}

	head, branch, err := gitHeadInfo(target, b.caps)
	if err != nil {
		return gitCloneResult{}, err
	}
	relDir, err := filepath.Rel(relRoot, target)
	if err != nil {
		relDir = dir
	}
	return gitCloneResult{Dir: filepath.ToSlash(relDir), Head: head, DefaultBranch: branch, Cwd: displayCwd(b.cwd)}, nil
}

// gitHeadInfo: reads short HEAD sha and branch name.
func gitHeadInfo(dir string, caps workspace.Caps) (head, branch string, err error) {
	out, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--short", "HEAD"}, caps, nil)
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(out)
	out, _, err = runGit(context.Background(), dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, caps, nil)
	if err != nil {
		return "", "", err
	}
	branch = strings.TrimSpace(out)
	return head, branch, nil
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

// Section markers for git_worktree_create/remove and git_pull/rebase.
