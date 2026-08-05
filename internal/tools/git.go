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

// maxGitOutputBytes caps how much of git's stdout/stderr any tool returns -
// the isolation model's generic "output caps" for the git tools (fs tools have
// their own dedicated read/write caps; git's text output shares this one).
const maxGitOutputBytes = 64 * 1024

// defaultCloneDepth is git_clone's default shallow depth when `depth` is unset.
const defaultCloneDepth = 1

// GitAskpassTokenEnv / GitAskpassUserEnv are the env vars quack sets ONLY on a
// git child process that needs to authenticate: askpass mode reads them to
// answer git's credential prompts. Never set on the quack server process
// itself - only injected per git exec.Command invocation (see gitEnv) - so
// they never appear in `ps` output for the long-lived quack process, only for
// the short-lived git child.
const (
	GitAskpassTokenEnv = "QUACK_GIT_ASKPASS_TOKEN"
	GitAskpassUserEnv  = "QUACK_GIT_ASKPASS_USERNAME"
)

// GitAskpassLinkName is the basename of the symlink the git tools maintain at
// the workspace root, pointing at the quack binary itself. GIT_ASKPASS must be
// a SINGLE executable path - git execs it directly with the prompt as one
// argument, with NO shell splitting of the value (setting it to
// "<binary> git-askpass" makes git look for a file literally named
// "quack git-askpass"; this exact failure shipped once). The symlink gives the
// binary a second name, and cmd/quack's main dispatches on
// filepath.Base(os.Args[0]) == GitAskpassLinkName BEFORE cobra (the busybox
// argv[0] pattern) - a real executable path, no shell, no secret on disk.
const GitAskpassLinkName = ".quack-askpass"

// GitAskpassAnswer answers one git credential prompt (git's two-call
// protocol: it invokes askpass once with a "Username for '<url>':" prompt and
// once with "Password for '<url>':") from the child-process-only env vars.
// Username prompts get the configured username (default x-access-token);
// anything else - password prompts, or a prompt-less probe - gets the token.
// Shared by the argv[0]-dispatch askpass mode and the hidden cobra
// subcommand (cmd/quack/git_askpass.go).
func GitAskpassAnswer(prompt string) string {
	if strings.Contains(strings.ToLower(prompt), "username") {
		return os.Getenv(GitAskpassUserEnv)
	}
	return os.Getenv(GitAskpassTokenEnv)
}

// ensureAskpassLink guarantees <root>/.quack-askpass is a symlink to the
// current quack binary, creating or repairing it as needed (a stale link -
// e.g. after the binary moved between deployments - is replaced). Returns the
// absolute link path for GIT_ASKPASS. Tolerates concurrent creation: if
// another goroutine wins the Symlink race, the winner's link is verified and
// used.
func ensureAskpassLink(root string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("git: resolve own binary path for GIT_ASKPASS: %w", err)
	}
	link := filepath.Join(root, GitAskpassLinkName)
	if dest, err := os.Readlink(link); err == nil && dest == self {
		return link, nil
	}
	_ = os.Remove(link) // stale target, or not a symlink - replace
	if err := os.Symlink(self, link); err != nil {
		// A concurrent creator may have won; accept its link if it's correct.
		if dest, rerr := os.Readlink(link); rerr == nil && dest == self {
			return link, nil
		}
		return "", fmt.Errorf("git: create askpass symlink %q: %w", link, err)
	}
	return link, nil
}

// gitAuth is a resolved credential ready for injection into one git child
// process: the credential plus the askpass symlink path GIT_ASKPASS points at.
type gitAuth struct {
	cred    GitCredential
	askpass string
}

// GitTokenSource dynamically mints a per-host git credential for a clone/remote
// URL - e.g. a GitHub App installation token, resolved from the URL's
// owner/repo and cached until shortly before expiry (see internal/github). It
// is consulted by authFor ONLY when no static workspace.git_credentials entry
// matches, so the static-PAT path stays backward compatible and wins. Returning
// (nil, nil) means "not my host" and the operation proceeds unauthenticated.
type GitTokenSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// authFor resolves the credential for rawURL's host - a static
// workspace.git_credentials entry first (credentialFor), else a dynamic
// GitTokenSource (the extension seam, e.g. a GitHub App installation token) -
// and, when one exists, ensures the askpass symlink so the returned auth is
// directly injectable. nil (no error) when no credential resolves for the host:
// the operation proceeds unauthenticated. A symlink failure is an error, not a
// silent unauthenticated fallback: the caller asked for an authenticated
// operation and degrading it quietly would just yield a confusing 401/prompt
// failure from git instead.
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

// GitCredential is one deployment-level per-host HTTPS credential
// (workspace.git_credentials in quack.yaml - see internal/config). Matching is
// by exact host (case-insensitive); Token is always an interpolated env value,
// never a literal secret in the YAML (config.Load enforces this - see
// validateNoLiteralTokens).
type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// gitBinding is the (userID, jail, caps, credentials, push-enabled) tuple every
// git tool closes over at construction - mirrors fsBinding's "no identity
// parsing inside tool handlers" rule (see fs.go).
type gitBinding struct {
	userID      string
	jail        *workspace.Jail
	caps        workspace.Caps
	credentials []GitCredential
	tokenSource GitTokenSource // optional dynamic credential source (extension seam)
	allowPush   bool
	// cwd is the session working directory (NODE-relative) a per-call copy carries -
	// set by withCwd from ctx state, mirroring fsBinding. A relative `dir`/`path` a
	// git tool takes resolves against it (resolve).
	cwd string
	// chatID is the per-chat scope (the workflow/chat session id) this call's
	// paths resolve under - set by withCwd from the advisor-thread marker in ctx
	// (scopeFromContext), mirroring fsBinding. "" resolves the per-user root.
	chatID string
	// nodeDir is the calling node's invisible root within that chat scope, applied
	// only at resolve time (jailPath) - which is what lands git_clone's default
	// target inside the NODE's dir instead of the shared chat root, without the dir
	// ever appearing in the reported path. Mirrors fsBinding.
	nodeDir string
}

// resolve is the cwd-, node- and chat-aware Jail.Resolve every git tool uses for
// its `dir`/`path` argument: relative to b.cwd under the node's own root,
// "/"-prefixed to the chat root (jailPath), scoped under b.chatID, always
// containment-checked by Jail.Resolve.
func (b gitBinding) resolve(p string) (string, error) {
	return b.jail.Resolve(b.userID, b.chatID, jailPath(b.nodeDir, b.cwd, p))
}

// credentialFor returns the configured credential matching rawURL's host
// (exact host match, case-insensitive), or nil when none is configured - the
// operation proceeds unauthenticated (public repos keep working with zero
// config; see the design doc's "Git auth" section).
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

// ---------------------------------------------------------------------------
// runGit: the ONE internal helper every git tool executes through.
// ---------------------------------------------------------------------------

// gitBinaryPath resolves the `git` executable via PATH once. A missing git
// binary is a deployment error (add it to the runtime image - see the
// Dockerfile), surfaced clearly the first time any git tool runs rather than
// probed at startup (git tools may never be registered for a given agent set).
func gitBinaryPath() (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git: the git binary is not installed in this image: %w", err)
	}
	return p, nil
}

// gitEnv builds the scrubbed environment for a git child process: no
// terminal-prompt fallback (a hung askpass would block the server), no
// system/global gitconfig (hermetic), a minimal PATH, and HOME pinned to
// caps.HomeDir when set - OUTSIDE any cloned repo, so a global config write
// or a hook shelling out can't land where `git add -A` would sweep it up -
// falling back to the repo dir only when unset. When auth is non-nil,
// GIT_ASKPASS points at the workspace-root symlink back to THIS binary (git
// execs it DIRECTLY as a program path, never "<binary> <subcommand>"), and
// the token travels ONLY as env vars on this one child process - never on
// disk, in a URL, or in `ps` output.
// gitChildPath mirrors workspace's childPath for git children: the fixed
// minimal PATH plus the operator's workspace.exec_path extras (a git hook or
// filter may legitimately need the configured toolchain).
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
	// workspace.env, same as run_command/checks children - a hook or filter
	// shelling out to a configured toolchain needs it findable too.
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

// capOutput truncates s to max bytes, the git tools' shared "output caps"
// (loud truncation, never an error - mirrors the fs tools' truncated-result
// convention, just folded into the text itself here since git's own textual
// output has no natural place for a separate `truncated` bool per call).
func capOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// runGit executes git as a subprocess: exec.Command argv arrays ONLY, never a
// shell (see the design doc's "never a shell" rule - this is what makes a
// force-push or an arbitrary --upload-pack unexpressible, not merely
// filtered), cwd pinned to dir (which callers resolve through the jail before
// calling this), env scrubbed (gitEnv), a per-call timeout from caps, and
// output capped. auth, when non-nil, is injected via GIT_ASKPASS for this one
// invocation only (see gitEnv / authFor).
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
	// Hooks neutralized: an agent-written .git/hooks/* would otherwise run
	// unsandboxed via this unconfined git binary.
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

// ---------------------------------------------------------------------------
// git_clone
// ---------------------------------------------------------------------------

type gitCloneResult struct {
	Dir           string `json:"dir"`
	Head          string `json:"head"`
	DefaultBranch string `json:"default_branch"`
	// Cwd is where you are standing. NOTE: `dir` is relative to it - the clone
	// landed at <cwd>/<dir>, and `cd` into it with exactly that `dir`.
	Cwd string `json:"cwd"`
}

// validateCloneURL enforces https-only and rejects any URL carrying
// credentials (user:pass@ or user@) - a token in a clone URL would persist
// into .git/config and every log line forever. See the design doc's "Git
// auth" section.
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

// defaultCloneDir derives a target directory name from a repo URL's last path
// segment, stripping a trailing ".git".
func defaultCloneDir(u *url.URL) string {
	name := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		name = "repo"
	}
	return name
}

// cloneRepo is the clone itself, past git_clone's URL validation: resolve the
// target through the jail, build the argv, run it. Split out so the clone path
// is exercisable against a local (file://) remote in tests.
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
	// A brand-new user's jail dir may not exist yet (the jail resolves paths
	// without creating them), and runGit's cwd must exist before git runs.
	if err := os.MkdirAll(relRoot, 0o755); err != nil {
		return gitCloneResult{}, fmt.Errorf("git_clone: create workspace dir: %w", err)
	}

	depth := defaultCloneDepth // absent → shallow
	if depthArg != nil {
		depth = *depthArg // explicit 0 (or negative) → full history
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

// gitHeadInfo reads a repo's current short HEAD sha and branch name.
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

// ---------------------------------------------------------------------------
// git_checkout
// ---------------------------------------------------------------------------

// validateRef rejects a ref that would be read by git as an OPTION rather than
// a ref (anything leading with "-", e.g. "--upload-pack=…"). argv-only exec
// means a ref can never become a shell command, but it could still smuggle a
// git flag into the argv - this is the other half of that guard.
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

// ---------------------------------------------------------------------------
// git_status
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_diff
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_log
// ---------------------------------------------------------------------------

// gitLogFieldSep/gitLogRecordSep are unit/record separators (ASCII 0x1f/0x1e)
// used to delimit git log's %h/%an <%ae>/%aI/%s fields - subjects and author
// names can contain almost anything else, so a printable delimiter (comma,
// pipe) would be ambiguous.
const (
	gitLogFieldSep  = "\x1f"
	gitLogRecordSep = "\x1e"
)

// ---------------------------------------------------------------------------
// git_commit
// ---------------------------------------------------------------------------

// GitCommitAuthorName/Email fix every quack commit's author AND committer to
// a system identity - commits are attributable to the system, not
// impersonating the user (see the design doc). Exported: internal/github's
// CI auto-heal reads this back off a failing commit to tell its own fix
// apart from a human's (see cifix.go's one-attempt guard).
const (
	GitCommitAuthorName  = "quack"
	GitCommitAuthorEmail = "agent@quack.local"
)

// maxAddAllFiles is the bulk-commit sanity wall: a blind `git add -A` that
// stages more files than this in one commit almost certainly swept in
// something outside the intended change (e.g. a hermetic child's $HOME
// pinned into the repo, so `npm ci` wrote its cache there too - see
// workspace's HomeDir fix for the other half). Deliberately a plain count,
// not judge/LLM guidance: commit message quality is judge territory, but
// "did we just stage a thousand files nobody asked for" is a fact a
// threshold answers directly.
const maxAddAllFiles = 100

// ---------------------------------------------------------------------------
// git_branch
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_push
// ---------------------------------------------------------------------------

// protectedBranches can NEVER be pushed to by an agent - humans merge into
// them. Force-push is unexpressible (no argv path ever adds --force); this is
// the other half of "the one outward-facing, non-undoable operation" guard.
var protectedBranches = map[string]bool{"main": true, "master": true}

// gitRemoteURL reads the "origin" remote's URL - used both to pick a matching
// credential and, implicitly, to confirm a remote is even configured.
func gitRemoteURL(dir string, caps workspace.Caps) (string, error) {
	out, _, err := runGit(context.Background(), dir, []string{"remote", "get-url", "origin"}, caps, nil)
	if err != nil {
		return "", fmt.Errorf("git: no \"origin\" remote configured: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// PushBranch pushes branch from the local clone at dir to "origin", credential
// injected via the SAME askpass mechanism the git tools use (never a URL, never
// written to disk) - the delivery step's own transient, App-authed push
// (internal/github), run OUTSIDE any agent's tool call and after judge pass, so
// it never races or duplicates a worker's own git activity. Mirrors git_push's
// safety rules: never force-pushes, rejects a protected branch outright.
// jailRoot anchors the askpass symlink (workspace.Jail.Root()).
func PushBranch(ctx context.Context, jailRoot, dir, branch string, cred GitCredential, caps workspace.Caps) (sha string, err error) {
	if protectedBranches[branch] {
		return "", fmt.Errorf("git: pushing to %q is rejected - a human merges", branch)
	}
	link, err := ensureAskpassLink(jailRoot)
	if err != nil {
		return "", err
	}
	auth := &gitAuth{cred: cred, askpass: link}
	// --force: the work branch is quack's own attempt branch and a re-run
	// SUPERSEDES the prior attempt - each run starts from a fresh setup clone,
	// so a leftover remote branch (a delivery that pushed then failed later,
	// a re-triggered label run) is never a fast-forward. Never main/master
	// (refused above); plain --force because a fresh clone holds no
	// remote-tracking ref for the branch, which is what --force-with-lease
	// would need.
	if _, _, err := runGit(ctx, dir, []string{"push", "--quiet", "--force", "origin", branch}, caps, auth); err != nil {
		return "", err
	}
	out, _, err := runGit(ctx, dir, []string{"rev-parse", "--short", branch}, caps, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ---------------------------------------------------------------------------
// git_worktree_create / git_worktree_remove
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_pull / git_rebase - auto-abort-on-conflict + report
// ---------------------------------------------------------------------------
