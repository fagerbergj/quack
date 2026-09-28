package workspace

import (
	"context"
	"fmt"
	"hash/maphash"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// GitProtocol is the only transport quack's own git calls may use; tests widen it to "file" for local fixtures.
var GitProtocol = "https"

// gitConfigKeep is every repo-config key quack's own git honours. Repo url.<base>.insteadOf, remote.<url>.url and
// filter/merge drivers redirect or run code and no -c counter-rule outranks them, so everything else is dropped.
var gitConfigKeep = regexp.MustCompile(`^(core\.(repositoryformatversion|filemode|bare|logallrefupdates|ignorecase|precomposeunicode|symlinks)|extensions\.(objectformat|refstorage)|remote\.origin\.(url|fetch)|branch\..+\.(remote|merge)|user\.(name|email))$`)

// gitConfigLocks serializes in-place strips per config path (striped by hash, so bounded) so two quack ops
// on one clone, e.g. fanned-out reviewer worktrees, don't unset each other's keys mid-list.
var (
	gitConfigLocks [64]sync.Mutex
	gitConfigSeed  = maphash.MakeSeed()
)

// GitSafeArgs are -c overrides, which beat every config file; each protocol is named because
// protocol.<name>.allow outranks protocol.allow. For a read-only query these neutralize the config-driven
// exec vectors (hooks, fsmonitor, ssh, gpg, gc) without the file strip GitCmd does for in-repo ops.
func GitSafeArgs() []string {
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
		"-c", "commit.gpgSign=false",
		"-c", "gpg.program=/bin/false",
		"-c", "gc.auto=0",
	)
}

// GitCmd builds a quack-internal git child that reads no agent-writable config: fresh empty HOME, no
// system/global config, and for an in-repo call (dir != "") the repo config first stripped to gitConfigKeep -
// the drivers/rewrites -c can't override. dir "" runs in that empty HOME. The returned func removes it.
func GitCmd(ctx context.Context, bin, dir string, argv, env []string) (*exec.Cmd, func(), error) {
	home, err := os.MkdirTemp("", "quack-git-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("git: create empty HOME: %w", err)
	}
	done := func() { _ = os.RemoveAll(home) }
	env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if dir, env, err = pinRepo(ctx, bin, dir, home, env); err != nil {
		done()
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, bin, append(GitSafeArgs(), argv...)...)
	cmd.Dir, cmd.Env = dir, env
	return cmd, done, nil
}

// pinRepo stops repo discovery at the symlink-resolved dir (home for ""), so a missing .git can't aim the
// strip at an enclosing repo's config; git compares ceilings against resolved paths.
func pinRepo(ctx context.Context, bin, dir, home string, env []string) (string, []string, error) {
	inRepo := dir != ""
	if !inRepo {
		dir = home
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", nil, fmt.Errorf("git: resolve %s: %w", dir, err)
	}
	parent := filepath.Dir(abs)
	if strings.ContainsRune(parent, os.PathListSeparator) {
		return "", nil, fmt.Errorf("git: %s contains %q, which would split GIT_CEILING_DIRECTORIES", parent, os.PathListSeparator)
	}
	env = append(env, "GIT_CEILING_DIRECTORIES="+parent)
	if inRepo {
		err = sanitizeGitConfig(ctx, bin, abs, env)
	}
	return abs, env, err
}

// sanitizeGitConfig unsets every key outside gitConfigKeep in dir's (common) repo config. Fails closed
// when dir is not a repo or the config file is not regular.
func sanitizeGitConfig(ctx context.Context, bin, dir string, env []string) error {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, append(GitSafeArgs(), args...)...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.Output()
		return string(out), err
	}
	common, err := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("git: locate repo config in %s: %w", dir, err)
	}
	cfg := filepath.Join(strings.TrimSpace(common), "config")
	if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("git: repo config %s is not a regular file", cfg)
	}
	mu := &gitConfigLocks[maphash.String(gitConfigSeed, cfg)%uint64(len(gitConfigLocks))]
	mu.Lock()
	defer mu.Unlock()
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
