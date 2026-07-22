# Workspace

`workspace:` configures the agents' working disk — the filesystem, git, and `run_command` tools all resolve through it. Every field is optional; omitting the section entirely still gets a working jail rooted at `./workspace` with sane defaults.

```yaml
workspace:
  root: ${QUACK_WORKSPACE_ROOT}   # default ./workspace
  max_read_kb: 256
  max_write_kb: 2048
  max_results: 200
  max_list_entries: 500
  timeout_seconds: 60
  sandbox: bwrap
  check_commands: ["go build", "go vet", "go test", "npm run", "npm test", "npx tsc", "make"]
```

## Two different boundaries

It's worth being precise about this, because the two boundaries protect against different things:

**The jail** is a path check on the *tools* (`internal/workspace`'s one path-resolution function). Every filesystem/git operation resolves inside a per-user directory under `root` (`<root>/<user_id>/` — `"local"` today, the OIDC subject later); `..`, absolute paths, and symlinks pointing outside the jail are all rejected. It never constrained a *child process* — only the `cwd` a tool handed it.

**The OS sandbox** (`workspace.sandbox`) is the boundary every `run_command` / gate-check *child process* runs inside. `bwrap` (the default) wraps each child in a [bubblewrap](https://github.com/containers/bubblewrap) namespace: a read-only view of the system dirs toolchains need, plus exactly two writable paths — the child's own working directory and its isolated `$HOME`. Nothing else exists in there: not `~/.ssh`, not `~/.aws`, not another node's clone. Because that namespace *is* the wall, `run_command` gives the model a real shell inside it (`/bin/sh -c "<command>"`) — pipes, redirects, `$()`, all of it works, and quack doesn't bother rejecting shell metacharacters on that path (they were never a real wall; `sh -c "…"` doesn't contain any of them).

If `bwrap` is configured but not usable — a container runtime whose seccomp profile blocks `unshare(CLONE_NEWUSER)`, for instance — **the server refuses to start**. That's deliberate: a silent fallback to no sandbox would be worse than a loud boot failure. `sandbox: none` runs children with the server user's full filesystem authority and logs a `WARN` on every start; it's the honest setting when the whole server is already confined to a throwaway container/VM (nested bwrap namespaces don't work inside most container runtimes). Under `none`, `run_command` falls back to argv-only and does reject shell metacharacters, since the habit guard is all there is left.

Every `run_command`/check/git child also gets its own `$HOME` (`<root>/<user_id>/.quack-home`) — a sibling of any cloned repo, never the repo's own working directory, so a toolchain's cache writes (npm's `_cacache`, `~/.gitconfig`) never land inside a git working tree where a later commit's `add_all` could sweep them up.

## Per-child resource limits

```yaml
workspace:
  limits:
    address_space_mb: 8192   # RLIMIT_AS, per process
    max_procs: 512           # RLIMIT_NPROC — bwrap only
    max_file_size_mb: 1024   # RLIMIT_FSIZE
```

Applied via `prlimit(1)` as the innermost wrapper (Go's `os/exec` has no `setrlimit` hook). A runaway build shouldn't be able to take the host down with it. `address_space_mb` is deliberately generous — Node's V8 reserves a large *virtual* region at startup, so tightening this doesn't slim a build down, it just stops Node running at all. `max_procs` only applies under `sandbox: bwrap`: `RLIMIT_NPROC` is counted per-UID system-wide, so outside the sandbox's user namespace a limit below the server's existing process count would fail every `fork()`. `0` on any of these means "inherit the server's own limit."

## `check_commands`: the guard checks run

`check_commands` is the allowlist of command **prefixes** the planner may complete into a code-implementer node's `checks`, and that the trust gate's deterministic stage derives a repo's own build/vet/test commands from when the planner sets none (see [trust-gate.md](trust-gate.md)). Each derived check is further gated on its binary existing on the host, so a runtime without `go`/`npm` just derives nothing rather than failing every node. An explicit `check_commands: []` disables checks entirely; the default is the list shown above.

## `git_credentials` and `git_push`

```yaml
workspace:
  # git_credentials:
  #   - host: github.com
  #     username: x-access-token
  #     token: ${QUACK_GITHUB_TOKEN}
  git_push: false
```

`git_credentials` is deployment-level, one HTTPS identity per host. `token` must be an `${VAR}` reference in the raw YAML — a literal here is a startup error, checked on the raw file text before `${VAR}` expansion, so it can't slip through as a "just for now" secret. Never put a credential in a clone URL; `git_clone` rejects that outright.

`git_push` gates the one outward-facing, non-undoable git operation. Even when `true`, `git_push` can never force-push (unexpressible — no argv path ever adds `--force`) and refuses `main`/`master`.

## The guard ladder

```yaml
workspace:
  guards:
    run_command: judge+confirm
    git_push: judge+confirm
```

`guards` maps a tool name to a tier: `none` (default for anything unlisted) | `judge` | `confirm` | `judge+confirm`. Tier 0 — the jail, the OS sandbox, no force-push/no-push-to-main — always applies underneath any guard result; it's never what decides *what* a command may do, only *where* it can reach.

- `judge` runs an independent safety-judge model call (reusing `gates.judge`'s provider/model) before the tool executes; a denial returns the refusal as the tool's result and the tool never runs.
- `confirm` pauses the DAG node for a human approve/deny, riding the same mid-node pause/resume path as a worker's `ask_user` question.

quack's shipped defaults guard `delete_path: judge`, `git_rebase: judge`, `git_push: judge+confirm`, and `run_command: judge+confirm` — the sandbox contains *where* a command reaches, the judge is what contains *what* it does. An off-task or exfiltrating command inside a perfectly good sandbox is still an off-task command.

**Since the code agents moved to external ACP subprocesses** (see [agents.md](agents.md#native-agents-vs-external-acp-agents)), the native fs/git write tools this ladder used to guard mostly don't exist anymore for those agents — delivery (including `git_push`) is gate-owned and runs after the trust gate, never inside the subprocess itself. `config/quack.yaml` ships `guards:` empty by default for that reason; the walls that remain are the jail on the surviving read tools and the OS sandbox around gate-check children.

### A webhook / autonomous deployment must drop `git_push` off `confirm`

The `confirm` tier pauses the run for a *human* to approve or deny. A webhook-triggered run — `quack:implement` on an issue, an auto-review on PR open — has no human sitting in the loop to answer that pause; a `confirm`-gated `git_push` in that path would just hang forever. That's why delivery in the ACP world is gate-owned rather than tool-gated: the trust gate decides whether the work is good enough to push, and a gate-failed PR opens as a draft instead of pausing for a confirmation nobody's there to give. If you do re-introduce a `confirm`-tiered tool into an autonomous path, make sure something on that path can actually answer the pause, or budget for the run timing out.
