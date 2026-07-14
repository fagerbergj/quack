# Code mode

Normally a model emits **one tool call per turn**, waits, then emits the next. Every
intermediate result lands in its context. Reading five files costs five round trips and
five files' worth of context.

In code mode the model instead **writes a program** that calls the tools as ordinary
functions. The program runs; **one** result comes back. A single turn can read five files,
grep, and write a patch — with real loops and conditionals — and the five file contents
never enter the model's context at all.

This exists because of a live failure: a `code-implementer` node spent 25 minutes and 98
tool calls re-reading the same files and wrote nothing, because a 65k context window could
not hold what it needed to hold.

## The one tool

Code mode is a single tool, `run_code`, whose **description is the API**:

```js
// run_code({ code: "..." })
const hits = grep({ pattern: "func Build", path: "internal" });
const files = [...new Set(hits.matches.map(m => m.path))].slice(0, 5);
const sizes = {};
for (const f of files) {
  try {
    sizes[f] = read_file({ path: f }).content.split("\n").length;
  } catch (e) {
    sizes[f] = "unreadable: " + e.message;
  }
}
return { files, sizes };
```

- Every tool the agent has is a plain **synchronous** function in the script's scope, named
  exactly as the tool is named, taking one object argument.
- The script is the **body of a function**: use `return`.
- `console.log(...)` is captured.
- A failing tool call **throws** a catchable JS exception, so a script can handle partial
  failure itself instead of dying.

The result carries `result` (the return value, JSON), `logs`, `calls` (a compact record of
every in-script call), and `error` if the script threw — with the line number, so the model
can fix its own script.

## The API is generated, never hand-written

The callable surface is built from each tool's real `Declaration()` — its name, description
and parameter schema, which ADK infers from the tool's own Go argument struct — and each JS
function is bound straight to **that same tool object's** `Run`.

There is exactly one source of truth. A tool whose schema changes changes the code-mode API
in the same commit, because there is no second copy to forget. This is enforced
structurally: `TestNoHandMaintainedToolList` parses `internal/tools/run_code.go` and fails if
a single string literal in it is the name of a registered tool.

## The guards still hold

`registry.go` assembles `run_code` **last**, over the tools it has already built and already
wrapped in the guard ladder (`guard.go`) and the per-node cancel guard (`cancelguard.go`). A
script's call invokes that same wrapped tool object, so the path jail, the OS sandbox, the
safety judge, the cancel guard and the workspace caps all apply to every in-script call for
free. None of it is reimplemented, so none of it can be bypassed.

The script itself gets **no ambient capability at all**: the runtime is
[goja](https://github.com/dop251/goja), a pure-Go ES interpreter running in-process — no
cgo, no subprocess, no node or python dependency. There is no filesystem, no network, no
`require`, no `process`. A script can call the functions we bound and nothing else. That is
a *stronger* sandbox than the shell it replaces, not a weaker one.

Two bounds stop a runaway script: a **60s wall clock** (enforced by goja's interrupt, so even
`while(true){}` is stopped) and a **200-call cap**.

## The ledger guarantee

This is the part that matters most.

The trust gate checks a worker's claims against an **activity ledger**, and that ledger is
built by scanning session events for `FunctionCall`/`FunctionResponse` pairs. **A tool called
inside a script produces no session event.** Left alone, a script that wrote files and
committed them would be *invisible* to the gate: the node failed for claiming work with no
evidence, or — far worse — real work passing unverified.

So `run_code`'s result carries `calls`: a compact record of every in-script call. The
scanner in `internal/vetting/node.go` **expands** it, replaying each recorded call through
the very same recorders a direct call goes through — the ledger entry, the commit SHA, the
clone dirs, the pushed flag, the written paths, `cd`'s cwd tracking.

**After that expansion, a file written from inside a script is indistinguishable, to the
gate, from one written by a direct `write_file` call.** `internal/vetting/run_code_ledger_test.go`
asserts exactly that, by comparing the two side by side rather than against a hand-written
expectation — so the two paths cannot drift apart without a test failure.

## Enabling it per agent

`run_code` is just another entry in an agent's `tools:` list in `config/quack.yaml` (and
`config/managed.yaml`):

```yaml
  code-explorer:
    tools: [read_file, list_dir, glob, grep, cd, git_clone, ..., run_code]
```

It is on for `code-implementer` and `code-explorer`. Every other agent is unchanged. The
one-tool-per-turn path stays exactly as it is — code mode **adds** a path, it never removes
one, and every tool remains callable the normal way.

## The honest limits

- **Confirm-tier and long-running tools are not in the script API.** A script has nowhere to
  suspend to: a mid-script human pause has no turn boundary to land on, and resuming would
  re-run the script from the top, re-doing every side effect it had already performed. Under
  the default guard config that means **`run_command` and `git_push` are not callable from
  inside a script** (both are `judge+confirm`), nor is `ask_user`. They stay fully available
  as ordinary one-call-per-turn tools. A deployment that sets `run_command: judge` gets it in
  code mode too. `judge`-tier tools (`delete_path`, `git_rebase`) *are* in the API — a denial
  simply comes back as the call's result.
- **An in-script read carries no content sample.** The compact record elides bulky payloads
  (`content`, `output`, `matches`, `entries`, …) down to a size or a count — that is the
  whole point, and a size fallback catches any bulky field not named, so a new tool cannot
  reintroduce the leak. The consequence is that the judge learns *that* a file was read
  (which backs a citation of it) but has no sample to spot-check a quoted line against. An
  in-script read is therefore a slightly weaker grounding signal than a direct one. Writes are
  unaffected: the judge re-reads changed files from disk.
- **In-script calls are not streamed to the UI.** A direct tool call surfaces as
  `agent_tool_call` / `agent_tool_result` SSE events. The calls inside a script do not; the
  frontend sees one `run_code` call and one result. The `calls` record is in that result, so
  nothing is lost to the gate or to an audit — but the live activity view is coarser for a
  node running in code mode.
- **JavaScript only.** goja is an ES5.1+ interpreter with much of ES6. It is not Node: there
  is no standard library beyond the language itself and `console`.
