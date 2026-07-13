---
name: plan-work
description: >
  How to decompose a request into a DAG of specialist agents and submit it to the
  plan tool. Load this BEFORE authoring any plan — it holds the common-workflow
  catalog and the rules for building a correct DAG.
---

# Plan Work

You turn a user request into the MINIMAL DAG of agent tasks that fully answers it,
then submit it to the `plan` tool as `nodes`. Pick agents by their exact names
from the **Agents** list in your system prompt.

## Common workflows

Match the request to a known shape first; fall back to the general rules below.

| Request | DAG shape |
| --- | --- |
| Single information topic | ONE `web-researcher` node, no synthesizer |
| Several distinct information topics | one `web-researcher` per topic → ONE `synthesizer` (final) |
| Has an `[User attached: ...]` file | a media node (see Media routing) first; chain to research/synthesis only if a factual question is also asked |
| Write/fix/refactor code in a repo | ONE `code-implementer` node — `checks` + `workdir` are MANDATORY whenever checks are available (see Code checks) |
| Review a PR / diff / branch / proposed change (read-only, no edits) | ONE `code-reviewer` node — it reads and critiques, never commits |
| Explore / understand / analyze a codebase or repo's structure, conventions, or how something is implemented (read-only, no edits) | ONE `code-explorer` node — it clones and reads, cites files, never commits |
| Add/implement a feature AND commit / push / open a PR | ONE `code-implementer` node whose task runs the WHOLE deliverable — clone, study conventions, implement + tests, run checks, commit, push a branch, open the PR (see Implement-and-deliver) |

Route by what the node must DO, not by topic: any node that must change code,
commit, or push is `code-implementer` work — never `web-researcher`, which
cannot commit and whose vetting expects web citations. A coding request may
still take an upstream `web-researcher` node when live web facts are genuinely
needed first.

A node that must **understand a codebase** (explore/analyze a repo's structure,
conventions, or how something is implemented — read-only, no edits) is
`code-explorer` work, NOT `web-researcher`: the explorer's sources are the files
it reads (cited `<repo>@<path>`), and it's judged on exploration quality —
code-grounding, accuracy, usefulness — not on web citations. Routing repo-
understanding to `web-researcher` fails it against a web-citation rubric it can
never satisfy. (For a single-repo *coding* task, still prefer folding "understand
the repo" INTO the `code-implementer`'s own task rather than a separate node —
see Implement-and-deliver below; reach for a standalone `code-explorer` node when
understanding the repo IS the deliverable, or when several downstream nodes share
the same repo understanding.)

## Implement-and-deliver requests

When the request is to **create / add / implement / write / fix / build** code AND
**commit / push / open a PR / submit** it, the DELIVERABLE is the committed-and-pushed
code and the opened PR — NOT an analysis of how one would do it. Two rules:

- The **terminal** node MUST be a `code-implementer` node, and its `task` MUST cover
  the full end-to-end deliverable: clone the repo, study its conventions, implement
  the change with test coverage, make it pass the repo's checks, commit, push a
  branch, and open the PR. Whenever checks are available, that node MUST also carry
  `checks` + `workdir` (see Code checks) — non-negotiable for code that ships.
- A "understand the repo / its conventions" step is at most an UPSTREAM feeder node —
  NEVER the terminal node and NEVER a substitute for the implementation. For a
  single-repo coding task, prefer folding "understand the repo" INTO the
  code-implementer's own task: it has `git_clone` + `read_file` + `grep` and is told
  to explore before writing, so a separate analyze node is usually redundant.

Worked example — input: *"Add a Flappy Bird game to repo R and open it as a PR; it
must fit the repo's conventions, pass its checks, and include tests for the game
logic."*

- CORRECT — ONE `code-implementer` node: task = "Clone R, study its structure and
  conventions, implement a Flappy Bird game that fits them with tests for the game
  logic, run the repo's typecheck/lint/tests until green, commit, push a branch, and
  open a pull request." — plus `checks` (build + typecheck + the game-logic tests) and
  `workdir` set, since the repo's check commands are available.
- WRONG — a lone `web-researcher` node that "analyzes the repo and reports the file
  tree, technologies, and build/lint/test commands." It fails because the deliverable
  was the code and the PR, not a report; `web-researcher` cannot clone-edit-commit-push,
  and the run "completes" having done none of the actual work. (The plan tool rejects a
  plan like this: an implement-and-deliver request with no `code-implementer` node.)

### Write the implement node's task as research → plan → implement

A code-implementer task that jumps straight to writing code ships a fragment —
isolated logic with no page, no wiring, that unit-tests green but isn't the
feature. Write the node's `task` so it front-loads understanding before any code
(this pairs with the implementer's own "complete vertical slice" standard — keep
them consistent, don't re-teach its workflow):

1. **Research first.** Study the repo's own conventions AND find a **sibling
   feature of the same kind**, so the implementer knows the *complete structure*
   a done deliverable has — entry point/route/page, sub-components, the
   registration that makes it appear in the app, metadata, and tests.
2. **Plan the full slice.** Enumerate every file and wiring point the sibling has
   before writing, so nothing (the page, the registration, the metadata) is left
   out.
3. **Implement + test + verify** against the node's `checks` until green.

Keep this as task *wording*, not extra nodes. The default is still ONE
`code-implementer` node doing the research inline — it has the live repo in its
own session (`git_clone` + `read_file` + `grep`), so a separate feeder node is
redundant for a focused change in a conventional repo. Escalate to a standalone
`code-explorer` feeder → implementer only when understanding the codebase is
substantial work in its own right (see `references/breaking-down-large-work.md`,
"When to split understanding from implementation").

## How to build the DAG

For a LARGE or multi-phase coding request where the decomposition isn't obvious — a
whole app or game, several distinct mechanics/screens, a feature touching many layers,
or a migration across many files — load `references/breaking-down-large-work.md` for a
full decision procedure (find seams, size nodes, vertical vs horizontal slicing, waves,
anti-patterns). For everything else, the steps below are enough.

Work through these in order:

1. **Understand the request.** Identify every distinct thing asked for. If it says
   "recent / latest / current / this year", scope tasks to the present and name the
   year explicitly (today's date is in your Environment section) rather than relying
   on training data.

2. **Choose the shape.** One focused job per node — a single question or a few
   tightly-related sub-questions. Never pack unrelated topics into one node
   ("research X, Y, and Z"); split them. A task that reads as a list of unrelated
   things is overloaded.

3. **Extract shared work.** If two+ nodes would each need the same underlying
   finding (the same entities, the same background), pull it into its OWN upstream
   node and have the dependents `depends_on` it — don't repeat it in each.

4. **Wire dependencies.** `depends_on: []` only when nodes are TRULY independent
   (each answerable without the other's output). Use `depends_on: [id]` when a node
   needs another's specific output (find which models exist, THEN look up their
   specs). The `synthesizer` depends on ALL other nodes (the plan tool enforces
   this, but author it correctly anyway).

5. **Write self-contained tasks** — the rule that most often breaks plans. Each node
   is a STATELESS worker that sees ONLY the `task` you write — not this conversation,
   not the other nodes' work. Resolve every reference ("this", "that", "the above")
   into explicit content. For a follow-up that transforms a prior answer (clean up,
   reformat, shorten, translate), QUOTE the relevant prior text inside the task.

## Code checks (`checks` + `workdir` on a node)

`checks` are commands the trust gate runs against a `code-implementer` node's
work after each draft — a failing check hard-fails vetting and its real
compiler/test output feeds the revise prompt, so the implementer iterates
against actual failures, not a reviewer's paraphrase. Without them, nothing
deterministically proves the deliverable even builds, and a judge alone can
pass incomplete or non-compiling code.

Rules:

- **Every `code-implementer` node MUST carry `checks` + `workdir` whenever the
  deployment has check commands available** (the `plan` tool's description lists
  the allowed prefixes). This is not optional — a code node with no checks while
  checks are available is rejected at submission. OMIT `checks`/`workdir` ONLY
  when the description says checks are **unavailable** (no prefixes configured).
- The checks MUST cover **build + typecheck + the tests nearest the change**, so
  an incomplete or non-compiling deliverable (logic with no wiring, a type error,
  a missing file) hard-fails deterministically instead of riding on the judge.
  Give the checks that actually verify the change, not every prefix available.
- Each check must be exactly one of the allowed prefixes, or extend one with
  arguments after a space (`go test` → `go test ./...`). Pipes are fine
  (`go vet ./... | head -50` — run natively, no shell). Anything else a shell
  would interpret (`& ; $ < > \` ( )`) rejects the whole plan at submission.
- Set `workdir` to the workspace-relative directory the checks should run in —
  the repo the node clones or edits (e.g. `repo`, matching the `dir` its task
  tells it to clone into). Name that directory explicitly in the task text so
  the node and the checks agree on it.
- Research and synthesis nodes never carry checks.

## Media routing

When the user message contains `[User attached: ...]`, pick ONE media agent:

- **audio/\*** → always `media-reader` (only it has ears).
- **image/\*** that is handwriting, cursive, dense text, small print, multi-column,
  or degraded/blurry, or asks to "transcribe" → `image-reader`.
- **image/\*** otherwise (general description, screenshot, identification) → `media-reader`.

The chosen node receives the actual file bytes; write its task as a specific
instruction. If a factual question is also asked, chain: media node → `web-researcher`
→ `synthesizer`.

## Submitting

Call `plan` with `nodes`, each `{id, agent, task, depends_on: [...]}` (optional
`rubric`; optional `checks` + `workdir` on a code node — see Code checks). The
tool validates and returns a `plan_id` and a summary — review it, then pass
`plan_id` to `execute`. If validation fails, fix the nodes and call again.
