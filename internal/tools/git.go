package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/workspace"
)

// maxGitOutputBytes caps how much of git's stdout/stderr any tool returns —
// the isolation model's generic "output caps" for the git tools (fs tools have
// their own dedicated read/write caps; git's text output shares this one).
const maxGitOutputBytes = 64 * 1024

// defaultCloneDepth is git_clone's default shallow depth when `depth` is unset.
const defaultCloneDepth = 1

// GitAskpassTokenEnv / GitAskpassUserEnv are the env vars quack sets ONLY on a
// git child process that needs to authenticate: askpass mode reads them to
// answer git's credential prompts. Never set on the quack server process
// itself — only injected per git exec.Command invocation (see gitEnv) — so
// they never appear in `ps` output for the long-lived quack process, only for
// the short-lived git child.
const (
	GitAskpassTokenEnv = "QUACK_GIT_ASKPASS_TOKEN"
	GitAskpassUserEnv  = "QUACK_GIT_ASKPASS_USERNAME"
)

// GitAskpassLinkName is the basename of the symlink the git tools maintain at
// the workspace root, pointing at the quack binary itself. GIT_ASKPASS must be
// a SINGLE executable path — git execs it directly with the prompt as one
// argument, with NO shell splitting of the value (setting it to
// "<binary> git-askpass" makes git look for a file literally named
// "quack git-askpass"; this exact failure shipped once). The symlink gives the
// binary a second name, and cmd/quack's main dispatches on
// filepath.Base(os.Args[0]) == GitAskpassLinkName BEFORE cobra (the busybox
// argv[0] pattern) — a real executable path, no shell, no secret on disk.
const GitAskpassLinkName = ".quack-askpass"

// GitAskpassAnswer answers one git credential prompt (git's two-call
// protocol: it invokes askpass once with a "Username for '<url>':" prompt and
// once with "Password for '<url>':") from the child-process-only env vars.
// Username prompts get the configured username (default x-access-token);
// anything else — password prompts, or a prompt-less probe — gets the token.
// Shared by the argv[0]-dispatch askpass mode and the hidden cobra
// subcommand (cmd/quack/git_askpass.go).
func GitAskpassAnswer(prompt string) string {
	if strings.Contains(strings.ToLower(prompt), "username") {
		return os.Getenv(GitAskpassUserEnv)
	}
	return os.Getenv(GitAskpassTokenEnv)
}

// ensureAskpassLink guarantees <root>/.quack-askpass is a symlink to the
// current quack binary, creating or repairing it as needed (a stale link —
// e.g. after the binary moved between deployments — is replaced). Returns the
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
	_ = os.Remove(link) // stale target, or not a symlink — replace
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
// URL — e.g. a GitHub App installation token, resolved from the URL's
// owner/repo and cached until shortly before expiry (see internal/github). It
// is consulted by authFor ONLY when no static workspace.git_credentials entry
// matches, so the static-PAT path stays backward compatible and wins. Returning
// (nil, nil) means "not my host" and the operation proceeds unauthenticated.
type GitTokenSource interface {
	GitCredential(ctx context.Context, rawURL string) (*GitCredential, error)
}

// authFor resolves the credential for rawURL's host — a static
// workspace.git_credentials entry first (credentialFor), else a dynamic
// GitTokenSource (the extension seam, e.g. a GitHub App installation token) —
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
// (workspace.git_credentials in quack.yaml — see internal/config). Matching is
// by exact host (case-insensitive); Token is always an interpolated env value,
// never a literal secret in the YAML (config.Load enforces this — see
// validateNoLiteralTokens).
type GitCredential struct {
	Host     string
	Username string
	Token    string
}

// gitBinding is the (userID, jail, caps, credentials, push-enabled) tuple every
// git tool closes over at construction — mirrors fsBinding's "no identity
// parsing inside tool handlers" rule (see fs.go).
type gitBinding struct {
	userID      string
	jail        *workspace.Jail
	caps        workspace.Caps
	credentials []GitCredential
	tokenSource GitTokenSource // optional dynamic credential source (extension seam)
	allowPush   bool
}

// newGitBinding resolves Deps into a gitBinding, defaulting caps when unset.
// Deps.Workspace nil is an error (a git tool listed for an agent without
// workspace: configured is a config mistake, not a silent no-op) — mirrors
// newFSBinding.
func newGitBinding(d Deps) (gitBinding, error) {
	if d.Workspace == nil {
		return gitBinding{}, fmt.Errorf("tools: git tools require workspace to be configured (see workspace.root in quack.yaml)")
	}
	userID := d.WorkspaceUserID
	if userID == "" {
		return gitBinding{}, fmt.Errorf("tools: git tools require a WorkspaceUserID")
	}
	caps := d.WorkspaceCaps
	if caps.IsZero() {
		caps = workspace.DefaultCaps()
	}
	return gitBinding{
		userID: userID, jail: d.Workspace, caps: caps,
		credentials: d.GitCredentials, tokenSource: d.GitTokenSource, allowPush: d.GitPush,
	}, nil
}

// credentialFor returns the configured credential matching rawURL's host
// (exact host match, case-insensitive), or nil when none is configured — the
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
// binary is a deployment error (add it to the runtime image — see the
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
// terminal-prompt fallback (a hung askpass prompt would otherwise block the
// server), no system/global gitconfig (hermetic — behavior never depends on
// what's on the host), a minimal PATH (just enough to find git's own helper
// binaries, e.g. git-remote-https), and HOME pinned to caps.HomeDir when set
// (the isolated per-user home OUTSIDE any cloned repo — see workspace.Jail.
// HomeDir), falling back to the repo dir itself only when unset. Pinning HOME
// to the repo dir unconditionally was the live bug this closes: a global git
// config/credential write (or a git hook shelling out to npm/pip) would land
// straight inside the repo tree, right where `git add -A` could sweep it up.
// When auth is non-nil, GIT_ASKPASS points at the workspace-root symlink back
// to THIS quack binary (see GitAskpassLinkName — git execs the value DIRECTLY
// as a single program path, so it must be a real executable, never
// "<binary> <subcommand>"), and the username/token travel ONLY as env vars on
// this one child process — never written to disk, never in a URL, never in
// `ps` output for the long-lived server process.
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
// (loud truncation, never an error — mirrors the fs tools' truncated-result
// convention, just folded into the text itself here since git's own textual
// output has no natural place for a separate `truncated` bool per call).
func capOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// runGit executes git as a subprocess: exec.Command argv arrays ONLY, never a
// shell (see the design doc's "never a shell" rule — this is what makes a
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
	cmd := exec.CommandContext(cctx, bin, argv...)
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

type gitCloneArgs struct {
	URL string `json:"url"`
	Dir string `json:"dir,omitempty"`
	// Depth is a pointer so an EXPLICIT 0 ("full history") is distinguishable
	// from absent ("default shallow depth 1") — the schema's contract.
	Depth *int `json:"depth,omitempty"` // default 1 (shallow); 0 = full
}

type gitCloneResult struct {
	Dir           string `json:"dir"`
	Head          string `json:"head"`
	DefaultBranch string `json:"default_branch"`
}

func newGitClone(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitCloneArgs, gitCloneResult](
		functiontool.Config{
			Name: "git_clone",
			Description: "Clone a git repository into your workspace. `url` must be a plain https:// URL with no " +
				"embedded credentials — configure a deployment-level credential (workspace.git_credentials) for " +
				"private repos instead. `dir` (workspace-relative) defaults to the repo name; `depth` defaults to " +
				"1 (a shallow clone) — pass 0 for full history.",
		},
		func(_ agent.Context, a gitCloneArgs) (gitCloneResult, error) { return b.gitClone(a) },
	)
}

// validateCloneURL enforces https-only and rejects any URL carrying
// credentials (user:pass@ or user@) — a token in a clone URL would persist
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

func (b gitBinding) gitClone(a gitCloneArgs) (gitCloneResult, error) {
	u, err := validateCloneURL(a.URL)
	if err != nil {
		return gitCloneResult{}, err
	}
	dir := a.Dir
	if dir == "" {
		dir = defaultCloneDir(u)
	}
	target, err := b.jail.Resolve(b.userID, dir)
	if err != nil {
		return gitCloneResult{}, err
	}
	userRoot, err := b.jail.Resolve(b.userID, "")
	if err != nil {
		return gitCloneResult{}, err
	}
	// A brand-new user's jail dir may not exist yet (the jail resolves paths
	// without creating them), and runGit's cwd must exist before git runs.
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		return gitCloneResult{}, fmt.Errorf("git_clone: create workspace dir: %w", err)
	}

	depth := defaultCloneDepth // absent → shallow
	if a.Depth != nil {
		depth = *a.Depth // explicit 0 (or negative) → full history
	}
	argv := []string{"clone", "--quiet"}
	if depth > 0 {
		argv = append(argv, "--depth", strconv.Itoa(depth))
	}
	argv = append(argv, a.URL, target)

	auth, err := b.authFor(a.URL)
	if err != nil {
		return gitCloneResult{}, err
	}
	if _, _, err := runGit(context.Background(), userRoot, argv, b.caps, auth); err != nil {
		return gitCloneResult{}, err
	}

	head, branch, err := gitHeadInfo(target, b.caps)
	if err != nil {
		return gitCloneResult{}, err
	}
	relDir, err := filepath.Rel(userRoot, target)
	if err != nil {
		relDir = dir
	}
	return gitCloneResult{Dir: filepath.ToSlash(relDir), Head: head, DefaultBranch: branch}, nil
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
// git_status
// ---------------------------------------------------------------------------

type gitStatusArgs struct {
	Dir string `json:"dir"`
}

type gitChange struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type gitStatusResult struct {
	Branch  string      `json:"branch"`
	Clean   bool        `json:"clean"`
	Changes []gitChange `json:"changes"`
}

func newGitStatus(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitStatusArgs, gitStatusResult](
		functiontool.Config{
			Name:        "git_status",
			Description: "Show a repository's current branch and working-tree changes. `dir` is the workspace-relative repo root.",
		},
		func(_ agent.Context, a gitStatusArgs) (gitStatusResult, error) { return b.gitStatus(a) },
	)
}

func (b gitBinding) gitStatus(a gitStatusArgs) (gitStatusResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitStatusResult{}, err
	}
	out, _, err := runGit(context.Background(), dir, []string{"status", "--porcelain=v1", "-b"}, b.caps, nil)
	if err != nil {
		return gitStatusResult{}, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	res := gitStatusResult{Clean: true}
	for i, ln := range lines {
		if i == 0 && strings.HasPrefix(ln, "## ") {
			res.Branch = parseBranchLine(ln)
			continue
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if len(ln) < 3 {
			continue
		}
		state := strings.TrimSpace(ln[:2])
		path := strings.TrimSpace(ln[3:])
		res.Changes = append(res.Changes, gitChange{Path: path, State: state})
		res.Clean = false
	}
	return res, nil
}

// parseBranchLine extracts the branch name from porcelain -b's "## " header,
// e.g. "## main...origin/main [ahead 1]" → "main"; "## HEAD (no branch)" (a
// detached HEAD) is returned verbatim.
func parseBranchLine(ln string) string {
	s := strings.TrimPrefix(ln, "## ")
	if i := strings.Index(s, "..."); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " "); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// git_diff
// ---------------------------------------------------------------------------

type gitDiffArgs struct {
	Dir  string `json:"dir"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
}

type gitDiffResult struct {
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

func newGitDiff(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitDiffArgs, gitDiffResult](
		functiontool.Config{
			Name: "git_diff",
			Description: "Show a diff. `ref` defaults to the worktree vs HEAD (uncommitted changes); pass a " +
				"commit/branch to diff against it instead (e.g. `HEAD~1`, `main`). `path` scopes the diff to one file.",
		},
		func(_ agent.Context, a gitDiffArgs) (gitDiffResult, error) { return b.gitDiff(a) },
	)
}

func (b gitBinding) gitDiff(a gitDiffArgs) (gitDiffResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitDiffResult{}, err
	}
	ref := a.Ref
	if ref == "" {
		ref = "HEAD"
	}
	argv := []string{"diff", ref}
	if a.Path != "" {
		argv = append(argv, "--", a.Path)
	}
	out, _, err := runGit(context.Background(), dir, argv, b.caps, nil)
	if err != nil {
		return gitDiffResult{}, err
	}
	truncated := strings.HasSuffix(out, "\n... (truncated)")
	return gitDiffResult{Diff: out, Truncated: truncated}, nil
}

// ---------------------------------------------------------------------------
// git_log
// ---------------------------------------------------------------------------

type gitLogArgs struct {
	Dir  string `json:"dir"`
	N    int    `json:"n,omitempty"` // default 20
	Path string `json:"path,omitempty"`
}

type gitCommitInfo struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

type gitLogResult struct {
	Commits []gitCommitInfo `json:"commits"`
}

// gitLogFieldSep/gitLogRecordSep are unit/record separators (ASCII 0x1f/0x1e)
// used to delimit git log's %h/%an <%ae>/%aI/%s fields — subjects and author
// names can contain almost anything else, so a printable delimiter (comma,
// pipe) would be ambiguous.
const (
	gitLogFieldSep  = "\x1f"
	gitLogRecordSep = "\x1e"
)

func newGitLog(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitLogArgs, gitLogResult](
		functiontool.Config{
			Name:        "git_log",
			Description: "Show recent commits. `n` defaults to 20. `path` scopes the log to one file's history.",
		},
		func(_ agent.Context, a gitLogArgs) (gitLogResult, error) { return b.gitLog(a) },
	)
}

func (b gitBinding) gitLog(a gitLogArgs) (gitLogResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitLogResult{}, err
	}
	n := a.N
	if n <= 0 {
		n = 20
	}
	format := "%h" + gitLogFieldSep + "%an <%ae>" + gitLogFieldSep + "%aI" + gitLogFieldSep + "%s" + gitLogRecordSep
	argv := []string{"log", "-n", strconv.Itoa(n), "--pretty=format:" + format}
	if a.Path != "" {
		argv = append(argv, "--", a.Path)
	}
	out, _, err := runGit(context.Background(), dir, argv, b.caps, nil)
	if err != nil {
		// An empty repo (no commits yet) is not an error — git log fails with
		// "does not have any commits yet"; surface an empty list instead.
		if strings.Contains(err.Error(), "does not have any commits yet") {
			return gitLogResult{}, nil
		}
		return gitLogResult{}, err
	}
	var commits []gitCommitInfo
	for _, rec := range strings.Split(out, gitLogRecordSep) {
		rec = strings.Trim(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		fields := strings.Split(rec, gitLogFieldSep)
		if len(fields) != 4 {
			continue
		}
		commits = append(commits, gitCommitInfo{SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]})
	}
	return gitLogResult{Commits: commits}, nil
}

// ---------------------------------------------------------------------------
// git_commit
// ---------------------------------------------------------------------------

type gitCommitArgs struct {
	Dir     string `json:"dir"`
	Message string `json:"message"`
	AddAll  *bool  `json:"add_all,omitempty"` // default true
	// Paths, when non-empty, stages exactly these workspace-relative paths
	// (`git add -- <paths>`) instead of `git add -A`'s blind "everything in
	// the tree" sweep — the escape hatch for a large but genuinely intentional
	// commit (vendoring, an initial scaffold): naming a path explicitly is
	// itself the sign the staging was deliberate, so it bypasses
	// maxAddAllFiles (see gitCommit). Ignored when AddAll is false.
	Paths []string `json:"paths,omitempty"`
}

type gitCommitResult struct {
	SHA          string `json:"sha"`
	FilesChanged int    `json:"files_changed"`
}

// gitCommitAuthorName/Email fix every quack commit's author AND committer to
// a system identity — commits are attributable to the system, not
// impersonating the user (see the design doc).
const (
	gitCommitAuthorName  = "quack"
	gitCommitAuthorEmail = "agent@quack.local"
)

// maxAddAllFiles is the bulk-commit sanity wall: a blind `git add -A` that
// stages more files than this in one commit almost certainly swept in
// something outside the intended change. Root cause of the live incident
// this guards against: a hermetic child's $HOME was pinned to the task's own
// cwd (the target repo), so `npm ci` wrote its cache directly into the repo
// tree, and `git add -A` then staged 1,261 cache files alongside 8 real ones
// in a single commit (see internal/workspace's HomeDir fix for the other half
// of this — this wall is the deterministic backstop for whatever still slips
// through, or a repo dirtied by something other than this run). Deliberately
// a plain count, not judge/LLM guidance — the user's own framing draws this
// line: commit NAME/message quality is judge territory (see agents/
// code-implementer/rubric.md's commit_hygiene), but "did we just stage a
// thousand files nobody asked for" is a fact a threshold answers directly.
const maxAddAllFiles = 100

func newGitCommit(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitCommitArgs, gitCommitResult](
		functiontool.Config{
			Name: "git_commit",
			Description: fmt.Sprintf("Commit staged (or, by default, all) changes. `add_all` defaults to true "+
				"(stages everything before committing); pass false to commit only what's already staged. "+
				"A blind add_all that would stage more than %d files is refused — pass `paths` naming exactly what "+
				"to stage instead (e.g. for an intentionally large commit like vendoring). "+
				"Every commit is attributed to %s <%s> — not the user.",
				maxAddAllFiles, gitCommitAuthorName, gitCommitAuthorEmail),
		},
		func(_ agent.Context, a gitCommitArgs) (gitCommitResult, error) { return b.gitCommit(a) },
	)
}

func (b gitBinding) gitCommit(a gitCommitArgs) (gitCommitResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitCommitResult{}, err
	}
	if strings.TrimSpace(a.Message) == "" {
		return gitCommitResult{}, fmt.Errorf("git_commit: message must not be empty")
	}
	addAll := a.AddAll == nil || *a.AddAll
	scoped := len(a.Paths) > 0 // explicit allowlist: caller named exactly what to stage
	if addAll {
		argv := []string{"add", "-A"}
		if scoped {
			argv = append([]string{"add", "--"}, a.Paths...)
		}
		if _, _, err := runGit(context.Background(), dir, argv, b.caps, nil); err != nil {
			return gitCommitResult{}, err
		}
	}
	staged, _, err := runGit(context.Background(), dir, []string{"diff", "--cached", "--name-only"}, b.caps, nil)
	if err != nil {
		return gitCommitResult{}, err
	}
	filesChanged := 0
	for _, ln := range strings.Split(staged, "\n") {
		if strings.TrimSpace(ln) != "" {
			filesChanged++
		}
	}
	// Bulk-commit sanity wall — only the blind add_all path (no explicit
	// `paths`) is gated; see maxAddAllFiles.
	if addAll && !scoped && filesChanged > maxAddAllFiles {
		_, _, _ = runGit(context.Background(), dir, []string{"reset"}, b.caps, nil) // best-effort: unstage, leave the tree as we found it
		return gitCommitResult{}, fmt.Errorf(
			"git_commit: add_all staged %d files, over the %d-file sanity limit — this usually means something "+
				"outside the intended change got swept in (a build/cache directory, an unrelated tree). Run "+
				"git_status to see what's staged; if this commit is genuinely meant to be this large, retry with "+
				"`paths` naming exactly what to stage instead of add_all", filesChanged, maxAddAllFiles)
	}
	argv := []string{
		"-c", "user.name=" + gitCommitAuthorName,
		"-c", "user.email=" + gitCommitAuthorEmail,
		"commit", "--quiet", "-m", a.Message,
	}
	if _, _, err := runGit(context.Background(), dir, argv, b.caps, nil); err != nil {
		return gitCommitResult{}, err
	}
	out, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--short", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitCommitResult{}, err
	}
	slog.Info("workspace mutation", "component", "tools", "tool", "git_commit", "user", b.userID, "dir", a.Dir, "files", filesChanged)
	return gitCommitResult{SHA: strings.TrimSpace(out), FilesChanged: filesChanged}, nil
}

// ---------------------------------------------------------------------------
// git_branch
// ---------------------------------------------------------------------------

type gitBranchArgs struct {
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"` // create+switch; absent = list
	From string `json:"from,omitempty"`
}

type gitBranchResult struct {
	Current  string   `json:"current"`
	Branches []string `json:"branches"`
}

func newGitBranch(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitBranchArgs, gitBranchResult](
		functiontool.Config{
			Name: "git_branch",
			Description: "List branches, or create+switch to a new one. Pass `name` to create a new branch " +
				"(optionally `from` a base ref, default HEAD) and switch to it; omit `name` to just list.",
		},
		func(_ agent.Context, a gitBranchArgs) (gitBranchResult, error) { return b.gitBranch(a) },
	)
}

func (b gitBinding) gitBranch(a gitBranchArgs) (gitBranchResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitBranchResult{}, err
	}
	if a.Name != "" {
		argv := []string{"checkout", "-b", a.Name}
		if a.From != "" {
			argv = append(argv, a.From)
		}
		if _, _, err := runGit(context.Background(), dir, argv, b.caps, nil); err != nil {
			return gitBranchResult{}, err
		}
	}
	out, _, err := runGit(context.Background(), dir, []string{"branch", "--format=%(refname:short)"}, b.caps, nil)
	if err != nil {
		return gitBranchResult{}, err
	}
	var branches []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			branches = append(branches, ln)
		}
	}
	cur, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitBranchResult{}, err
	}
	return gitBranchResult{Current: strings.TrimSpace(cur), Branches: branches}, nil
}

// ---------------------------------------------------------------------------
// git_push
// ---------------------------------------------------------------------------

type gitPushArgs struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch,omitempty"` // default: current
}

type gitPushResult struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// protectedBranches can NEVER be pushed to by an agent — humans merge into
// them. Force-push is unexpressible (no argv path ever adds --force); this is
// the other half of "the one outward-facing, non-undoable operation" guard.
var protectedBranches = map[string]bool{"main": true, "master": true}

func newGitPush(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitPushArgs, gitPushResult](
		functiontool.Config{
			Name: "git_push",
			Description: "Push the current (or named) branch to origin. Disabled by default " +
				"(workspace.git_push: true to enable); requires a configured credential for the remote's host; " +
				"NEVER force-pushes; pushing to main/master is always rejected — propose via a branch instead.",
		},
		func(_ agent.Context, a gitPushArgs) (gitPushResult, error) { return b.gitPush(a) },
	)
}

func (b gitBinding) gitPush(a gitPushArgs) (gitPushResult, error) {
	if !b.allowPush {
		return gitPushResult{}, fmt.Errorf("git_push: disabled — set workspace.git_push: true to enable")
	}
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitPushResult{}, err
	}
	branch := a.Branch
	if branch == "" {
		cur, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
		if err != nil {
			return gitPushResult{}, err
		}
		branch = strings.TrimSpace(cur)
	}
	if protectedBranches[branch] {
		return gitPushResult{}, fmt.Errorf("git_push: pushing to %q is rejected — propose changes via a branch; a human merges", branch)
	}
	remoteURL, err := gitRemoteURL(dir, b.caps)
	if err != nil {
		return gitPushResult{}, err
	}
	auth, err := b.authFor(remoteURL)
	if err != nil {
		return gitPushResult{}, err
	}
	if auth == nil {
		return gitPushResult{}, fmt.Errorf("git_push: no credential configured for this remote's host (see workspace.git_credentials)")
	}
	if _, _, err := runGit(context.Background(), dir, []string{"push", "--quiet", "origin", branch}, b.caps, auth); err != nil {
		return gitPushResult{}, err
	}
	sha, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--short", branch}, b.caps, nil)
	if err != nil {
		return gitPushResult{}, err
	}
	slog.Info("workspace mutation", "component", "tools", "tool", "git_push", "user", b.userID, "dir", a.Dir, "branch", branch)
	return gitPushResult{Remote: "origin", Branch: branch, SHA: strings.TrimSpace(sha)}, nil
}

// gitRemoteURL reads the "origin" remote's URL — used both to pick a matching
// credential and, implicitly, to confirm a remote is even configured.
func gitRemoteURL(dir string, caps workspace.Caps) (string, error) {
	out, _, err := runGit(context.Background(), dir, []string{"remote", "get-url", "origin"}, caps, nil)
	if err != nil {
		return "", fmt.Errorf("git: no \"origin\" remote configured: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// ---------------------------------------------------------------------------
// git_worktree_create / git_worktree_remove
// ---------------------------------------------------------------------------

type gitWorktreeCreateArgs struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
	Path   string `json:"path,omitempty"`
	From   string `json:"from,omitempty"`
}

type gitWorktreeCreateResult struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

func newGitWorktreeCreate(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitWorktreeCreateArgs, gitWorktreeCreateResult](
		functiontool.Config{
			Name: "git_worktree_create",
			Description: "Create a new git worktree with a new branch. `path` (workspace-relative) defaults to " +
				"`<repo>-wt-<branch>`, a sibling of the repo; `from` (default HEAD) is the base ref for the new branch.",
		},
		func(_ agent.Context, a gitWorktreeCreateArgs) (gitWorktreeCreateResult, error) {
			return b.gitWorktreeCreate(a)
		},
	)
}

func (b gitBinding) gitWorktreeCreate(a gitWorktreeCreateArgs) (gitWorktreeCreateResult, error) {
	if strings.TrimSpace(a.Branch) == "" {
		return gitWorktreeCreateResult{}, fmt.Errorf("git_worktree_create: branch must not be empty")
	}
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitWorktreeCreateResult{}, err
	}
	relPath := a.Path
	if relPath == "" {
		relPath = filepath.ToSlash(filepath.Join(filepath.Dir(a.Dir), filepath.Base(a.Dir)+"-wt-"+a.Branch))
	}
	wtPath, err := b.jail.Resolve(b.userID, relPath)
	if err != nil {
		return gitWorktreeCreateResult{}, err
	}
	userRoot, err := b.jail.Resolve(b.userID, "")
	if err != nil {
		return gitWorktreeCreateResult{}, err
	}
	argv := []string{"worktree", "add", "-b", a.Branch, wtPath}
	if a.From != "" {
		argv = append(argv, a.From)
	}
	if _, _, err := runGit(context.Background(), dir, argv, b.caps, nil); err != nil {
		return gitWorktreeCreateResult{}, err
	}
	relOut, err := filepath.Rel(userRoot, wtPath)
	if err != nil {
		relOut = relPath
	}
	slog.Info("workspace mutation", "component", "tools", "tool", "git_worktree_create", "user", b.userID, "dir", a.Dir, "branch", a.Branch)
	return gitWorktreeCreateResult{Path: filepath.ToSlash(relOut), Branch: a.Branch}, nil
}

type gitWorktreeRemoveArgs struct {
	Dir  string `json:"dir"`
	Path string `json:"path"`
}

type gitWorktreeRemoveResult struct {
	Removed bool `json:"removed"`
}

func newGitWorktreeRemove(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitWorktreeRemoveArgs, gitWorktreeRemoveResult](
		functiontool.Config{
			Name: "git_worktree_remove",
			Description: "Remove a git worktree. Refuses (errors) if the worktree has uncommitted changes — " +
				"delete or commit them first if you truly mean to discard it.",
		},
		func(_ agent.Context, a gitWorktreeRemoveArgs) (gitWorktreeRemoveResult, error) {
			return b.gitWorktreeRemove(a)
		},
	)
}

func (b gitBinding) gitWorktreeRemove(a gitWorktreeRemoveArgs) (gitWorktreeRemoveResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitWorktreeRemoveResult{}, err
	}
	wtPath, err := b.jail.Resolve(b.userID, a.Path)
	if err != nil {
		return gitWorktreeRemoveResult{}, err
	}
	statusOut, _, err := runGit(context.Background(), wtPath, []string{"status", "--porcelain"}, b.caps, nil)
	if err != nil {
		return gitWorktreeRemoveResult{}, err
	}
	if strings.TrimSpace(statusOut) != "" {
		return gitWorktreeRemoveResult{}, fmt.Errorf("git_worktree_remove: %q has uncommitted changes; commit or delete them first", a.Path)
	}
	if _, _, err := runGit(context.Background(), dir, []string{"worktree", "remove", wtPath}, b.caps, nil); err != nil {
		return gitWorktreeRemoveResult{}, err
	}
	if _, _, err := runGit(context.Background(), dir, []string{"worktree", "prune"}, b.caps, nil); err != nil {
		return gitWorktreeRemoveResult{}, err
	}
	slog.Info("workspace mutation", "component", "tools", "tool", "git_worktree_remove", "user", b.userID, "dir", a.Dir, "path", a.Path)
	return gitWorktreeRemoveResult{Removed: true}, nil
}

// ---------------------------------------------------------------------------
// git_pull / git_rebase — auto-abort-on-conflict + report
// ---------------------------------------------------------------------------

type gitPullArgs struct {
	Dir    string `json:"dir"`
	Rebase *bool  `json:"rebase,omitempty"` // default true — linear history
}

type gitPullResult struct {
	Branch    string   `json:"branch"`
	SHA       string   `json:"sha"`
	Updated   bool     `json:"updated"`
	Conflicts []string `json:"conflicts,omitempty"`
}

func newGitPull(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitPullArgs, gitPullResult](
		functiontool.Config{
			Name: "git_pull",
			Description: "Pull the current branch's upstream (`rebase` defaults to true — linear history). " +
				"On a conflict the pull is automatically ABORTED (the repo is left exactly as it was) and the " +
				"conflicting files are listed — resolve by inspecting them with your other tools and retrying, " +
				"never mid-conflict.",
		},
		func(_ agent.Context, a gitPullArgs) (gitPullResult, error) { return b.gitPull(a) },
	)
}

func (b gitBinding) gitPull(a gitPullArgs) (gitPullResult, error) {
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitPullResult{}, err
	}
	branchOut, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitPullResult{}, err
	}
	branch := strings.TrimSpace(branchOut)
	before, _, err := runGit(context.Background(), dir, []string{"rev-parse", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitPullResult{}, err
	}

	remoteURL, err := gitRemoteURL(dir, b.caps)
	if err != nil {
		return gitPullResult{}, err
	}
	auth, err := b.authFor(remoteURL)
	if err != nil {
		return gitPullResult{}, err
	}

	rebase := a.Rebase == nil || *a.Rebase
	argv := []string{"pull", "--quiet"}
	if rebase {
		argv = append(argv, "--rebase")
	}
	argv = append(argv, "origin", branch)

	if _, _, pullErr := runGit(context.Background(), dir, argv, b.caps, auth); pullErr != nil {
		conflicts, cerr := abortOnConflict(dir, b.caps, rebase)
		if cerr != nil {
			return gitPullResult{}, fmt.Errorf("git_pull: %w (and abort failed: %v)", pullErr, cerr)
		}
		if len(conflicts) > 0 {
			return gitPullResult{Branch: branch, SHA: strings.TrimSpace(before), Conflicts: conflicts}, nil
		}
		return gitPullResult{}, pullErr
	}
	after, _, err := runGit(context.Background(), dir, []string{"rev-parse", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitPullResult{}, err
	}
	return gitPullResult{
		Branch:  branch,
		SHA:     strings.TrimSpace(after),
		Updated: strings.TrimSpace(before) != strings.TrimSpace(after),
	}, nil
}

type gitRebaseArgs struct {
	Dir    string `json:"dir"`
	Onto   string `json:"onto"`
	Branch string `json:"branch,omitempty"`
}

type gitRebaseResult struct {
	SHA       string   `json:"sha"`
	Rebased   bool     `json:"rebased"`
	Conflicts []string `json:"conflicts,omitempty"`
}

func newGitRebase(d Deps) (tool.Tool, error) {
	b, err := newGitBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[gitRebaseArgs, gitRebaseResult](
		functiontool.Config{
			Name: "git_rebase",
			Description: "Rebase the current (or named) branch onto `onto` (e.g. `origin/main`; remote refs are " +
				"fetched first). On a conflict the rebase is automatically ABORTED (the repo is left exactly as " +
				"it was) and the conflicting files are listed. Never interactive — there is no editor to drive.",
		},
		func(_ agent.Context, a gitRebaseArgs) (gitRebaseResult, error) { return b.gitRebase(a) },
	)
}

func (b gitBinding) gitRebase(a gitRebaseArgs) (gitRebaseResult, error) {
	if strings.TrimSpace(a.Onto) == "" {
		return gitRebaseResult{}, fmt.Errorf("git_rebase: onto must not be empty")
	}
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return gitRebaseResult{}, err
	}
	if a.Branch != "" {
		if _, _, err := runGit(context.Background(), dir, []string{"checkout", "--quiet", a.Branch}, b.caps, nil); err != nil {
			return gitRebaseResult{}, err
		}
	}
	remoteURL, rerr := gitRemoteURL(dir, b.caps)
	var auth *gitAuth
	if rerr == nil {
		// Best-effort auth too: an askpass-symlink failure here degrades to an
		// unauthenticated fetch, matching the best-effort fetch below.
		auth, _ = b.authFor(remoteURL)
	}
	// Best-effort fetch: a purely-local rebase target (e.g. onto a local branch,
	// no remote configured) should still work, so a fetch failure here is not fatal.
	_, _, _ = runGit(context.Background(), dir, []string{"fetch", "--quiet", "origin"}, b.caps, auth)

	if _, _, rbErr := runGit(context.Background(), dir, []string{"rebase", a.Onto}, b.caps, nil); rbErr != nil {
		conflicts, cerr := abortOnConflict(dir, b.caps, true)
		if cerr != nil {
			return gitRebaseResult{}, fmt.Errorf("git_rebase: %w (and abort failed: %v)", rbErr, cerr)
		}
		if len(conflicts) > 0 {
			return gitRebaseResult{Conflicts: conflicts}, nil
		}
		return gitRebaseResult{}, rbErr
	}
	sha, _, err := runGit(context.Background(), dir, []string{"rev-parse", "--short", "HEAD"}, b.caps, nil)
	if err != nil {
		return gitRebaseResult{}, err
	}
	return gitRebaseResult{SHA: strings.TrimSpace(sha), Rebased: true}, nil
}

// abortOnConflict is the shared conflict policy for git_pull/git_rebase: on
// any failure, check for unmerged (conflicting) paths BEFORE aborting (abort
// discards that state), then run `rebase --abort` or `merge --abort` to leave
// the repo exactly as it was. Returns the sorted conflict file list (empty
// when the failure wasn't actually a conflict — e.g. a network error — so the
// caller falls back to surfacing the original error untouched).
func abortOnConflict(dir string, caps workspace.Caps, rebase bool) ([]string, error) {
	out, _, err := runGit(context.Background(), dir, []string{"diff", "--name-only", "--diff-filter=U"}, caps, nil)
	if err != nil {
		return nil, err
	}
	var conflicts []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			conflicts = append(conflicts, ln)
		}
	}
	if len(conflicts) == 0 {
		return nil, nil
	}
	sort.Strings(conflicts)
	abortArgv := []string{"merge", "--abort"}
	if rebase {
		abortArgv = []string{"rebase", "--abort"}
	}
	if _, _, err := runGit(context.Background(), dir, abortArgv, caps, nil); err != nil {
		return conflicts, err
	}
	return conflicts, nil
}
