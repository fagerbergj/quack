# `quack sandbox`

Constructs the EXACT jail an ACP agent gets - same `workspace.Caps`, same
`WrapArgv` wrapping, same `spawnEnv` - and either drops you into it or runs
one command in it. Before this existed, "does `go test ./...` pass in the
reviewer's sandbox" was only answerable by labeling a PR and waiting for a
live review to hit the wall.

```
quack sandbox [--agent NAME] [--cwd DIR] [--mode landlock|bwrap|none] [--keep]
quack sandbox run [same flags] "CMD ARGS"
quack sandbox check [same flags]
quack sandbox info [same flags]
```

- `quack sandbox` (no args) drops you into an interactive `sh -i` inside the
  jail, with a real pty crossing the sandbox boundary so job control and the
  prompt both work under bwrap and landlock. The prompt names the seat:
  `[quack:<agent> <ro|rw>] $`. Exiting tears down the scratch dir unless
  `--keep`.
- `run "CMD ARGS"` runs `sh -c "$ARG"` once, streaming stdout/stderr and
  passing the exit code through.
- `check` runs the probe table below and exits non-zero on any `FAIL`. This
  is what CI runs against the built image.
- `info` prints the resolved mode, cwd, tmp dir, home, RO/RW grants, `PATH`,
  and the full child env - no exec, for reading what an agent gets without
  running anything.

`--agent` defaults to `code-reviewer`. `--cwd` defaults to a fresh dir
minted under the workspace root; `--cwd .` jails the current directory
instead. `--mode` overrides the agent's configured sandbox (`none` on a dev
box that has no bwrap/landlock).

## Probe table

Each row is one probe from `internal/cli/sandbox_check.go`, run as `sandbox
run` under the hood:

| Probe | PASS means |
|---|---|
| write `$TMPDIR`, `$TMPDIR/sub/dir`, `$HOME` | the scratch/home grants are writable |
| write cwd | writable when the agent isn't read-only, `EACCES` when it is |
| `go env` GOTOOLCHAIN/GOMODCACHE/GOTMPDIR/GOCACHE | all set, all under writable paths |
| `go build` offline | a throwaway module builds under `$TMPDIR` with no network |
| `git init`+commit+push to a bare repo under `$TMPDIR` | no cross-device (EXDEV) error |
| `git clone --local` into `$TMPDIR` | the hardlink path works (same device) |
| `git push https://github.com/x/y` | fails, with no credential prompt |
| `unshare --user true`, `bwrap --version` | INFO only: whether nested namespaces are available |
| each `workspace.check_commands` entry | INFO only: whether the binary is on `ChildPath` |

Under `--mode none` there's no OS-enforced boundary, so the boundary-only
probes (cwd write, clone-allowed) degrade to `INFO` instead of `FAIL` - there
is nothing for them to have failed.

## Container reality

The real jail only exists inside the quack container, as uid 65532:

```
docker exec -it quack quack sandbox --agent code-reviewer   # interactive needs -it
docker exec quack quack sandbox check --agent code-reviewer
```

On a dev box, `--mode none` still makes `info` and `run` useful; `check`
reports the boundary probes as `INFO` rather than `FAIL`.
