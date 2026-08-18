// push.go: the gate-owned push (moved from internal/github's App.Deliver,
// which had no business running quack's own sandboxed git exec). The git
// plumbing is duplicated from internal/tools/git.go, not imported - tools
// already imports vetting, so the reverse import would cycle.
package vetting

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// GitCredential: per-host HTTPS credential for the gate-owned push.
type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// GitCredentialSource: resolves a push credential for a clone URL's host.
type GitCredentialSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// Must match internal/tools.GitAskpassTokenEnv/UserEnv/LinkName - the same
// quack binary (cmd/quack/git_askpass.go) answers either package's child.
const (
	gitAskpassTokenEnv  = "QUACK_GIT_ASKPASS_TOKEN"
	gitAskpassUserEnv   = "QUACK_GIT_ASKPASS_USERNAME"
	gitAskpassLinkName  = ".quack-askpass"
	maxGitPushOutputLen = 64 * 1024
)

// Branches the gate never pushes to. --force is unexpressible.
var protectedBranches = map[string]bool{"main": true, "master": true}

// stagesPush reports whether any staged item needs the branch pushed (#452).
func stagesPush(items []StagedDelivery) bool {
	for _, it := range items {
		if it.Kind == "pull_request" {
			return true
		}
	}
	return false
}

// ensurePush pushes before Deliver sees a pull-request-kind item, stamping
// dc.PushedSHA on success. No-op (not an error) when nothing stages a push.
func ensurePush(ctx context.Context, cfg Config, dc *DeliveryContext) error {
	if !stagesPush(dc.Items) || dc.Branch == "" || dc.CloneDir == "" {
		return nil
	}
	if cfg.GitCredentials == nil || cfg.Workspace == nil {
		return fmt.Errorf("push %q: no git credential source configured", dc.Branch)
	}
	cred, err := cfg.GitCredentials.GitCredential(ctx, dc.CloneURL)
	if err != nil {
		return fmt.Errorf("push %q: resolve credential: %w", dc.Branch, err)
	}
	if cred == nil {
		return fmt.Errorf("push %q: no credential available for %q", dc.Branch, dc.CloneURL)
	}
	sha, err := PushBranch(ctx, cfg.Workspace.Root(), dc.CloneDir, dc.Branch, *cred, workspace.DefaultCaps())
	if err != nil {
		return fmt.Errorf("push %q: %w", dc.Branch, err)
	}
	dc.PushedSHA = sha
	return nil
}

// gitAuth: resolved credential for one git child process.
type gitAuth struct {
	cred    GitCredential
	askpass string
}

// ensureAskpassLink: ensures .quack-askpass symlink to current binary; tolerates concurrent creation.
func ensureAskpassLink(root string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("git: resolve own binary path for GIT_ASKPASS: %w", err)
	}
	link := filepath.Join(root, gitAskpassLinkName)
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

// gitBinaryPath: resolves git via PATH.
func gitBinaryPath() (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git: the git binary is not installed in this image: %w", err)
	}
	return p, nil
}

// pushGitChildPath: scrubbed PATH for the push's git child.
func pushGitChildPath(caps workspace.Caps) string {
	base := "/usr/bin:/bin"
	if len(caps.ExtraPath) == 0 {
		return base
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + base
}

func pushGitEnv(dir string, caps workspace.Caps, auth *gitAuth) []string {
	home := caps.HomeDir
	if home == "" {
		home = dir
	}
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + pushGitChildPath(caps),
		"HOME=" + home,
		// Git writes loose objects via a tmp file under TMPDIR then renames it
		// into .git/objects - unset, that defaults to the real /tmp, which can
		// be a different device than dir and turn the rename into EXDEV (#936).
		"TMPDIR=" + workspace.SandboxTmpDir(caps),
	}
	if auth != nil {
		env = append(env,
			"GIT_ASKPASS="+auth.askpass,
			gitAskpassUserEnv+"="+auth.cred.Username,
			gitAskpassTokenEnv+"="+auth.cred.Token,
		)
	}
	return env
}

// capPushOutput: truncates to max bytes with a suffix.
func capPushOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// runPushGit: executes git as a subprocess with scrubbed env and capped output.
func runPushGit(ctx context.Context, dir string, argv []string, caps workspace.Caps, auth *gitAuth) (stdout, stderr string, err error) {
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

	env := pushGitEnv(dir, caps, auth)
	// Neutralize hooks (agent-written hooks would run unsandboxed).
	fullArgv := append([]string{"-c", "core.hooksPath=/dev/null"}, argv...)
	cmd := exec.CommandContext(cctx, bin, fullArgv...)
	cmd.Dir = dir
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	out := capPushOutput(outBuf.String(), maxGitPushOutputLen)
	errOut := capPushOutput(errBuf.String(), maxGitPushOutputLen)

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	slog.Info("git", "component", "vetting", "argv", strings.Join(argv, " "), "dir", dir, "exit", exitCode)

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

// PushBranch: force-pushes branch to origin (delivery step, outside any agent tool call).
func PushBranch(ctx context.Context, jailRoot, dir, branch string, cred GitCredential, caps workspace.Caps) (sha string, err error) {
	if protectedBranches[branch] {
		return "", fmt.Errorf("git: pushing to %q is rejected - a human merges", branch)
	}
	link, err := ensureAskpassLink(jailRoot)
	if err != nil {
		return "", err
	}
	auth := &gitAuth{cred: cred, askpass: link}
	// --force: each run starts fresh, so leftover remote is never a fast-forward.
	if pushErr := pushForce(ctx, dir, branch, caps, auth); pushErr != nil {
		// A branch surviving from a prior run on the same issue is normal (#714):
		// rebase onto it and retry once before giving up.
		if rerr := rebaseOntoRemote(ctx, dir, branch, caps, auth); rerr != nil {
			return "", pushErr
		}
		if pushErr = pushForce(ctx, dir, branch, caps, auth); pushErr != nil {
			return "", pushErr
		}
	}
	out, _, err := runPushGit(ctx, dir, []string{"rev-parse", "--short", branch}, caps, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func pushForce(ctx context.Context, dir, branch string, caps workspace.Caps, auth *gitAuth) error {
	_, _, err := runPushGit(ctx, dir, []string{"push", "--quiet", "--force", "origin", branch}, caps, auth)
	return err
}

// rebaseOntoRemote replays local commits on top of the surviving remote branch; aborts cleanly on conflict so a failed retry leaves the branch untouched.
func rebaseOntoRemote(ctx context.Context, dir, branch string, caps workspace.Caps, auth *gitAuth) error {
	if _, _, err := runPushGit(ctx, dir, []string{"fetch", "--quiet", "origin", branch}, caps, auth); err != nil {
		return err
	}
	if _, _, err := runPushGit(ctx, dir, []string{"rebase", "FETCH_HEAD"}, caps, nil); err != nil {
		_, _, _ = runPushGit(ctx, dir, []string{"rebase", "--abort"}, caps, nil)
		return err
	}
	return nil
}
