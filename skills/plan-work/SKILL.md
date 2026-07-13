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
| Write/fix/refactor code in a repo | ONE `code-implementer` node (the gate derives its checks from the repo — see Code checks) |
| Review a PR / diff / branch / proposed change (read-only, no edits) | ONE `code-reviewer` node — it reads and critiques, never commits |
| Explore / understand / analyze a codebase or repo's structure, conventions, or how something is implemented (read-only, no edits) | ONE `code-explorer` node — it clones and reads, cites files, never commits |
| Learn how ANOTHER project (a third-party OSS repo) implements something — "how does OpenHands do X?", "how does goose expose tools?" | ONE `code-explorer` node per project — it CLONES THEIR REPO and reads the real source. NOT `web-researcher`: articles and docs describe code, only the code is the code. Use `web-researcher` only for facts that exist nowhere but the web (a blog post's rationale, a spec, pricing) |
| Add/implement a feature AND commit / push / open a PR | ONE `code-implementer` node whose task runs the WHOLE deliverable — clone, study conventions, implement + tests, run checks, commit, push a branch, open the PR (see Implement-and-deliver) |
| Research several projects, THEN design, THEN implement and open a PR (a multi-phase request) | ONE DAG spanning ALL the phases: one `code-explorer` per project → (optionally ONE `synthesizer`) → ONE `code-implementer` (terminal). See Multi-phase requests |

**Multi-phase requests.** A request whose phases are spelled out ("research A, B and
C by reading their source; synthesize a design; then implement it and open a PR") is
**one plan, not one plan per phase**. Plan the whole job in a single DAG: the research
nodes are FEEDER steps and the terminal node is the one that produces the deliverable
(the `code-implementer` that commits, pushes, and opens the PR).

Do NOT plan only the first phase and stop. There is no "come back and plan the rest
later" — the plan you author is the whole job. A plan of research nodes for a request
that ends in "open a pull request" will be REJECTED (the terminal deliverable must be a
`code-implementer` node), and re-authoring it with *more research nodes* wastes turns:
the missing node is the implementer.

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
  branch, and open the PR. Do NOT guess `checks` for it — the gate derives them
  from the repo (see Code checks).
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
  open a pull request." — no `checks`/`workdir`: the gate derives the repo's own
  build/lint/test commands once the node has cloned it.
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
against actual failures, not a reviewer's paraphrase.

**`checks` are OPTIONAL, and you should almost always omit them.** Check commands
are a property of the REPO — and you have NOT seen the repo when you author the
plan. Guessing them produces nonsense (`go build` for a JavaScript repo; `npx
tsc` for a repo whose typecheck is `next build`). So don't guess: the trust gate
**derives** a code node's checks from the repo itself once the node has cloned it
— reading the repo's OWN `package.json` scripts (`npm run build`/`lint`/`test`),
or its `go.mod` (`go build ./...`, `go vet ./...`, `go test ./...`), or its
`Makefile` targets — and runs only the ones the deployment allows.

Rules:

- **Omit `checks` (and `workdir`) by default.** Set them ONLY when the user
  explicitly named the commands to run ("make sure `npm run e2e` passes"). A
  planner-set list is an explicit override and wins over derivation.
- When you do set them, each check must be exactly one of the allowed prefixes
  listed in the `plan` tool's description, or extend one with arguments after a
  space (`go test` → `go test ./...`). Pipes are fine (`go vet ./... | head -50` —
  run natively, no shell). Anything else a shell would interpret
  (`& ; $ < > \` ( )`) rejects the whole plan at submission.
- When you set `checks`, also set `workdir` — the workspace-relative directory
  they run in (e.g. `repo`, matching the `dir` the task tells the node to clone
  into). Name that directory explicitly in the task text so the node and the
  checks agree on it. (With no `workdir`, the gate finds the node's repo itself.)
- Checks — yours or the gate's derived ones — run against a **fresh clone**, which
  has none of the project's dependencies installed. The node's TASK must tell the
  implementer to install them first (`npm ci`, `go mod download`, …, whatever the
  repo uses); skip that and the checks fail closed with "command not found" and the
  node burns its revise budget on it.
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
