package workspace

// Ceiling of GitCmd's repo pinning: under landlock mode Landlock denies symlinks and alternates leading outside the
// clone; elsewhere they stay open. GC's jail-wide prune root stays open in every mode.

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// ErrNotQuackRepo marks a dir that fails GitCmd's repository checks.
var ErrNotQuackRepo = errors.New("not a repository quack created")

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
		// No child git in a nested repo (its config is never stripped); .gitmodules can outrank diff.ignoreSubmodules.
		"-c", "diff.ignoreSubmodules=all",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
	)
}

// gitConfined: GitCmd runs git under Landlock (see ConfineGit).
var (
	gitConfined        atomic.Bool
	gitUnconfinedNoted sync.Once
)

// ConfineGit sets whether quack's own git runs under Landlock: only in landlock mode, which ResolveSandbox has
// proved works here. Call once at boot with the resolved mode.
func ConfineGit(mode SandboxMode) {
	gitConfined.Store(mode == SandboxLandlock)
	if mode != SandboxLandlock {
		gitUnconfinedNoted.Do(func() {
			slog.Info("quack's own git runs unconfined: symlinks or alternates in an agent-writable .git can reach "+
				"other repositories; workspace.sandbox: landlock confines it", "component", "workspace", "sandbox", mode)
		})
	}
}

// GitCmd builds quack's own git child: empty per-call HOME (removed by the returned func), no system/global
// config, and for dir != "" the repo resolveRepo(clone, dir) pins, its config stripped to gitConfigKeep. rw names
// dirs the call writes outside that repo (a clone target, a new worktree); under ConfineGit they are created.
func GitCmd(ctx context.Context, bin, clone, dir string, argv, env []string, rw ...string) (*exec.Cmd, func(), error) {
	home, err := os.MkdirTemp("", "quack-git-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("git: create empty HOME: %w", err)
	}
	done := func() { _ = os.RemoveAll(home) }
	env = append(env, "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	g, err := newGitGrants(bin, home, env, rw)
	if g != nil {
		env = append(env, "TMPDIR="+home)
	}
	if err == nil {
		dir, env, err = pinRepo(ctx, g, bin, clone, dir, home, env)
	}
	if err != nil {
		done()
		return nil, nil, err
	}
	return gitCommand(ctx, g, bin, dir, argv, env), done, nil
}

// gitGrants is one confined git call's Landlock rule set; nil runs git unconfined.
type gitGrants struct {
	bin    string
	rw, ro []string
}

// newGitGrants: RW on HOME and rw only (pinRepo adds the repo), so a symlink or alternate leading anywhere else
// under the jail is denied. /dev/null is RW because git's run-command opens it O_RDWR; the resolv.conf file rule
// follows a systemd host's symlink into /run.
func newGitGrants(bin, home string, env, rw []string) (*gitGrants, error) {
	if !gitConfined.Load() {
		return nil, nil
	}
	for _, p := range rw {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, fmt.Errorf("git: create %s: %w", p, err)
		}
	}
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	g := &gitGrants{bin: bin, rw: append([]string{home, "/dev/null"}, rw...), ro: append(landlockSystemDirs(), "/dev", "/etc/resolv.conf", bin)}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_SSL_CAINFO", "GIT_SSL_CAPATH", "SSL_CERT_FILE", "SSL_CERT_DIR":
			g.ro = append(g.ro, v)
		case "GIT_ASKPASS":
			// The askpass link targets the quack binary; grant that, not whatever the link names.
			self, err := os.Executable()
			if err != nil {
				return nil, fmt.Errorf("git: resolve askpass binary: %w", err)
			}
			g.ro = append(g.ro, self)
		}
	}
	return g, nil
}

// gitCommand runs bin with GitSafeArgs then argv in dir, through the __sandbox-exec shim when g is set.
func gitCommand(ctx context.Context, g *gitGrants, bin, dir string, argv, env []string) *exec.Cmd {
	full := append(append([]string{bin}, GitSafeArgs()...), argv...)
	if g != nil {
		full[0] = g.bin
		full = assembleSandboxExec(g.rw, g.ro, full)
	}
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	cmd.Dir, cmd.Env = dir, env
	return cmd
}

// pinRepo hands git its repository explicitly (GIT_DIR/GIT_WORK_TREE/GIT_COMMON_DIR also override any commondir
// file) so nothing in the tree steers discovery. The no-repo call stops discovery at its resolved empty HOME.
func pinRepo(ctx context.Context, g *gitGrants, bin, clone, dir, home string, env []string) (string, []string, error) {
	if dir != "" {
		work, gitDir, common, err := resolveRepo(clone, dir)
		if err != nil {
			return "", nil, err
		}
		if g != nil {
			g.rw = append(g.rw, filepath.Dir(common), work)
		}
		env = append(env, "GIT_DIR="+gitDir, "GIT_WORK_TREE="+work, "GIT_COMMON_DIR="+common)
		return work, env, sanitizeGitConfig(ctx, g, bin, work, filepath.Join(common, "config"), env)
	}
	abs, err := realPath(home)
	if err != nil {
		return "", nil, fmt.Errorf("git: resolve %s: %w", home, err)
	}
	parent := filepath.Dir(abs)
	if strings.ContainsRune(parent, os.PathListSeparator) {
		return "", nil, fmt.Errorf("git: %s contains %q, which would split GIT_CEILING_DIRECTORIES", parent, os.PathListSeparator)
	}
	return abs, append(env, "GIT_CEILING_DIRECTORIES="+parent), nil
}

// resolveRepo pins clone's real .git as the only repo quack's git may touch; dir is clone or a linked worktree
// at <clone>/.git/worktrees/<name> whose commondir leads back. No alternates; paths are symlink-resolved.
func resolveRepo(clone, dir string) (work, gitDir, common string, err error) {
	if work, gitDir, common, err = checkRepo(clone, dir); err != nil {
		return "", "", "", fmt.Errorf("git: %s is %w at %s: %w", dir, ErrNotQuackRepo, clone, err)
	}
	return work, gitDir, common, nil
}

// RepoRedirect says why dir fails GitCmd's checks against clone; nil when it passes or is simply absent.
func RepoRedirect(clone, dir string) error {
	if _, _, _, err := resolveRepo(clone, dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func checkRepo(clone, dir string) (work, gitDir, common string, err error) {
	if clone == "" {
		return "", "", "", fmt.Errorf("no trusted clone given")
	}
	root, err := realPath(clone)
	if err != nil {
		return "", "", "", err
	}
	common = filepath.Join(root, ".git")
	if fi, err := os.Lstat(common); err != nil || !fi.IsDir() {
		return "", "", "", fmt.Errorf("%s is not a directory", common)
	}
	if work, err = realPath(dir); err != nil {
		return "", "", "", err
	}
	gitDir = common
	if work != root {
		if gitDir, err = worktreeGitDir(work, common); err != nil {
			return "", "", "", err
		}
	}
	// Quack never creates alternates, and a commondir in the main .git would reroute every ref and object.
	for _, f := range []string{"commondir", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(common, f)); !os.IsNotExist(err) {
			return "", "", "", fmt.Errorf("%s is present", filepath.Join(common, f))
		}
	}
	return work, gitDir, common, nil
}

// worktreeGitDir: work's .git pointer must name <common>/worktrees/<name>, and that gitdir's commondir must
// resolve back to common.
func worktreeGitDir(work, common string) (string, error) {
	gitDir, err := linkedGitDir(work)
	if err != nil {
		return "", err
	}
	if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
		return "", fmt.Errorf("worktree gitdir %s is outside %s", gitDir, filepath.Join(common, "worktrees"))
	}
	back, err := readPointer(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(back) {
		back = filepath.Join(gitDir, back)
	}
	if real, err := realPath(back); err != nil || real != common {
		return "", fmt.Errorf("worktree commondir %s does not lead back to %s", back, common)
	}
	return gitDir, nil
}

// linkedGitDir reads a linked worktree's .git pointer file, resolved.
func linkedGitDir(work string) (string, error) {
	p := filepath.Join(work, ".git")
	line, err := readPointer(p)
	if err != nil {
		return "", err
	}
	gitDir, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok {
		return "", fmt.Errorf("%s has no gitdir line", p)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(work, gitDir)
	}
	return realPath(gitDir)
}

// WorktreeClone returns the clone that owns linked worktree dir once the pair passes GitCmd's repo checks,
// refusing a clone outside root. "" when dir has no .git pointer file.
func WorktreeClone(root, dir string) (string, error) {
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || fi.IsDir() {
		return "", nil
	}
	gitDir, err := linkedGitDir(dir)
	if err != nil {
		return "", fmt.Errorf("git: worktree %s: %w", dir, err)
	}
	clone := filepath.Dir(filepath.Dir(filepath.Dir(gitDir)))
	r, err := realPath(root)
	if err != nil {
		return "", fmt.Errorf("git: resolve %s: %w", root, err)
	}
	if rel, err := filepath.Rel(r, clone); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("git: worktree %s belongs to %s, outside %s", dir, clone, r)
	}
	if _, _, _, err := resolveRepo(clone, dir); err != nil {
		return "", err
	}
	return clone, nil
}

// readPointer reads a small git pointer file; O_NONBLOCK plus the regular-file check keep a planted FIFO or
// device from blocking quack, and O_NOFOLLOW refuses a symlink.
func readPointer(p string) (string, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	return strings.TrimSpace(string(data)), err
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// sanitizeGitConfig unsets every key outside gitConfigKeep in the pinned repo's config cfg. Fails closed
// when cfg is not a regular file.
func sanitizeGitConfig(ctx context.Context, g *gitGrants, bin, dir, cfg string, env []string) error {
	git := func(args ...string) (string, error) {
		out, err := gitCommand(ctx, g, bin, dir, args, env).Output()
		return string(out), err
	}
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
