package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GitProtocol is the only transport quack's own git calls may use; tests widen it to "file" for local fixtures.
var GitProtocol = "https"

// gitConfigKeep is every repo-config key quack's own git honours: repo url.<base>.insteadOf and remote.<url>.url
// redirect even an explicit URL and no -c counter-rule outranks them, so everything else is dropped.
var gitConfigKeep = regexp.MustCompile(`^(core\.(repositoryformatversion|filemode|bare|logallrefupdates|ignorecase|precomposeunicode|symlinks)|extensions\.(objectformat|refstorage)|remote\.origin\.(url|fetch)|branch\..+\.(remote|merge)|user\.(name|email))$`)

// gitSafeArgs are -c overrides, which beat every config file; each protocol is named because
// protocol.<name>.allow outranks protocol.allow.
func gitSafeArgs() []string {
	args := []string{"-c", "protocol.allow=never"}
	for _, p := range []string{"file", "git", "ssh", "ext", "fd", "http", "ftp", "ftps"} {
		args = append(args, "-c", "protocol."+p+".allow=never")
	}
	return append(args,
		"-c", "protocol."+GitProtocol+".allow=always",
		"-c", "credential.helper=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.sshCommand=/bin/false",
	)
}

// GitCmd builds a quack-internal git child that reads no agent-writable config; a credentialed call first strips
// dir's repo config to gitConfigKeep. dir "" runs in the fresh empty HOME, which the returned func removes.
func GitCmd(ctx context.Context, bin, dir string, argv, env []string, credentialed bool) (*exec.Cmd, func(), error) {
	home, err := os.MkdirTemp("", "quack-git-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("git: create empty HOME: %w", err)
	}
	done := func() { _ = os.RemoveAll(home) }
	env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if dir == "" {
		dir = home
	} else if credentialed {
		if err := sanitizeGitConfig(ctx, bin, dir, env); err != nil {
			done()
			return nil, nil, err
		}
	}
	cmd := exec.CommandContext(ctx, bin, append(gitSafeArgs(), argv...)...)
	cmd.Dir, cmd.Env = dir, env
	return cmd, done, nil
}

// sanitizeGitConfig unsets every key outside gitConfigKeep in dir's repo config.
func sanitizeGitConfig(ctx context.Context, bin, dir string, env []string) error {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, append(gitSafeArgs(), args...)...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.Output()
		return string(out), err
	}
	common, err := git("rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("git: locate repo config in %s: %w", dir, err)
	}
	cfg := strings.TrimSpace(common)
	if !filepath.IsAbs(cfg) {
		cfg = filepath.Join(dir, cfg)
	}
	cfg = filepath.Join(cfg, "config")
	if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("git: repo config %s is not a regular file", cfg)
	}
	keys, err := git("config", "--file", cfg, "--no-includes", "--name-only", "--list", "-z")
	if err != nil {
		return fmt.Errorf("git: read repo config: %w", err)
	}
	seen := map[string]bool{}
	for _, k := range strings.Split(keys, "\x00") {
		if k == "" || seen[k] || gitConfigKeep.MatchString(k) {
			continue
		}
		seen[k] = true
		if _, err := git("config", "--file", cfg, "--unset-all", k); err != nil {
			return fmt.Errorf("git: drop repo config %q: %w", k, err)
		}
	}
	return nil
}
