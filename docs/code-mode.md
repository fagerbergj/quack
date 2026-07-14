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

## One approval, for one program

`run_code` is itself **one tool call**. So it pauses and confirms exactly like any other
tool — and that is where the guard goes. The safety judge and the human see the **script**:
a readable, deterministic program, approved **once, before a single line of it runs**.

```
model writes a script
  → run_code (judge+confirm)
      → safety judge reads the PROGRAM        → deny ⇒ it never runs; the refusal is the result
      → human approves the PROGRAM            → deny ⇒ it never runs
  → the script executes, once, start to finish
```

Inside an approved script, the tools run **without their individual confirm/judge guards**.
The program was already judged and approved as a whole; re-judging each call would be
redundant, and at the judge tier it would be a model call per loop iteration. What still
applies to **every** in-script call:

- **the path jail and the workspace caps** — these live in the tool *implementations*
  (`newFSBinding`, `newGitBinding`), below the guard wrapper, so removing the wrapper cannot
  touch them. `read_file("/etc/passwd")` fails inside a script exactly as it does outside one.
- **the OS sandbox** around `run_command`'s children (bubblewrap) — unchanged.
- **the per-node cancel guard** (`cancelguard.go`) — a cancelled node stops mid-script, within
  one call, even if the script swallows the error.
- **the activity ledger** — see below. Non-negotiable, and unchanged.

`registry.go` builds each tool **once** and hands out two views of it: the fully-wrapped tool
for the model's direct, one-call-per-turn use, and a script-bound view that skips the guard
ladder but keeps the cancel guard. Neither view is unguarded, and a tool cannot get one
without the other.

The invariant that keeps that honest as tools are added: **`run_code`'s tier is its own
configured tier raised to the union of the tiers of every tool it binds** (`tools.scriptTier`).
A script can do anything its tools can do, so it is always at least as guarded as the
most-guarded tool in it. Config can make `run_code` *more* guarded, never less, and a tool
given a guard tier tomorrow cannot become reachable through a script under a weaker guard than
its own — even if nobody remembers to touch `run_code`'s config entry.

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

Its guard tier is an ordinary `workspace.guards` entry, and it ships at `judge+confirm` —
the same tier `run_command` has, because a script that can run commands must be at least as
guarded as a command:

```yaml
  guards:
    run_command: judge+confirm
    run_code: judge+confirm
```

## What comes back is enforced, not requested

A script may read whatever it likes. What it RETURNS is bounded: any large string in the
return value that is verbatim payload a tool in that script already handed it is **elided**,
with a marker saying so, and the model is told what was dropped and how much.

This is enforcement rather than instruction because instruction demonstrably failed. The
tool description already said, in capitals, *"RETURN ONLY WHAT YOU NEED — NEVER THE FILE
CONTENTS"*, with a worked example — and code mode's very first live script returned 52.2 KB
of file contents anyway. Asking does not work.

No other harness enforces this. Cloudflare's Code Mode "relies entirely on the language
model's own judgment" (their own documentation); goose returns whatever its runtime printed.
They can afford to: on a 200k-context frontier model a dump is waste. On a 65k window it is
the failure the feature exists to prevent.

What is NOT elided, and must never be:

- a **computed** answer — a patch, a diff, a generated file. None is a verbatim substring of
  what a tool returned (its own markers and interleaving break containment), so the detector
  cannot mistake one for an echo. There is a test whose only job is to prove this; if it ever
  fails, eliding has started destroying real work and must be reverted.
- a **short quote** — a signature, a failing assertion, the three lines around a bug. That is
  an answer, not a dump.

If a script genuinely needs a file's full text in the model's context, `read_file` it
directly. Code mode is for the case where it does not.

## The honest limits

- **The human now approves a PROGRAM, not each command.** That is more power per approval —
  the real tradeoff of this design, and worth saying plainly. What makes it a *better* guard
  rather than a weaker one: the thing being approved is readable, deterministic text, shown in
  full before anything runs, and reviewed by the safety judge first. The alternative is N
  approvals of opaque individual calls, each stripped of the context of the others — which is
  how approval fatigue is manufactured, and how a human ends up rubber-stamping the tenth
  `run_command` of a sequence they can no longer reconstruct. One program, once, read whole,
  is the better bargain. (It is also the only one that works: with confirm-tier tools excluded,
  a script could not run the tests or push a branch, which is most of what code mode is *for*.)
- **A turn-ending tool is still not in the script API.** `ask_user` and `get_user_choice` end
  the model's turn and are answered on the *next* one; a script has no turn boundary inside it
  for the answer to arrive on, and an in-script `ask_user` emits no session event, so the gate
  would never even see the question. They stay fully available as ordinary one-call-per-turn
  tools, and the generated API says why they are absent. **Everything else is in the script
  API**, `run_command` and `git_push` included.
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
