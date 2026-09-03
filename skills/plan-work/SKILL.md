---
name: plan-work
description: >
  How to decompose a request into a DAG of specialist agents and submit it to the
  plan tool. Load this BEFORE authoring any plan - it holds the common-workflow
  catalog and the rules for building a correct DAG.
---

# Plan Work

You turn a user request into the MINIMAL DAG of agent tasks that fully answers it, then submit it to the `plan` tool as `nodes`. Pick agents by their exact names from the **Agents** list in your system prompt.

**Before submitting, ask this of your own plan: if it runs exactly as written, does it hand back what the request asked to receive?** Name the artifact the request wants, find the node whose own task text actually produces it, and check that node is the plan's TERMINAL (last, undepended-on) node. A terminal node tasked to "explore", "investigate", or "produce a report/findings/analysis" never satisfies a request whose deliverable is a plan, a review, or shipped code, no matter how thorough - exploration may only be a prerequisite feeding the node that produces the real artifact (a live failure: a single `code-explorer` node tasked to "produce a detailed report" was submitted, and accepted, for a request asking for an implementation plan - approach, files to change, how to verify). The reverse fails too: a plan that stops at exploring or reviewing when the request asked for shipped code. The plan judge (`internal/vetting/plan_judge.go`) asks this same question independently before your plan runs - a plan that fails it comes back rejected, wasting a re-plan round, so check it yourself first.

## Common workflows

The table below is a shortcut for common shapes, not a substitute for the check above - match the request to a known shape first, but still verify the terminal node produces the actual artifact asked for; fall back to the general rules below when nothing fits.

| Request | DAG shape |
| --- | --- |
| Single information topic | ONE `web-researcher` node, no synthesizer |
| Several distinct information topics | one `web-researcher` per topic → ONE `synthesizer` (final) |
| Has an `[User attached: ...]` file | a media node (see Media routing) first; chain to research/synthesis only if a factual question is also asked |
| Write/fix/refactor code in a repo | ONE `code-implementer` node (the gate derives its checks from the repo - see Code checks) |
| Review a PR / diff / branch / proposed change (read-only, no edits) | Default: ONE `code-reviewer` node for the whole diff. Fan out to MULTIPLE `code-reviewer` nodes (never `code-explorer`) only when the diff spans independent subsystems AND is large (~15+ files / ~800+ changed lines) - #948 merges their verdicts into one review. See Reviewing a PR |
| Explore / understand / analyze a codebase or repo's structure, conventions, or how something is implemented (read-only, no edits) | ONE `code-explorer` node - it clones and reads, cites files, never commits |
| Produce an implementation PLAN (not the code itself) - "plan how to add X", "how would you implement Y", a `quack:plan`-labeled issue | one or more `code-explorer`/`web-researcher` feeder nodes → a terminal `synthesizer` that writes the plan. Frame the feeder node's task as "explore AND turn what you learn into a phased plan", not bare "explore" - see When to add a synthesizer below |
| Learn how ANOTHER project (a third-party OSS repo) implements something - "how does OpenHands do X?", "how does goose expose tools?" | ONE `code-explorer` node per project - it CLONES THEIR REPO and reads the real source. NOT `web-researcher`: articles and docs describe code, only the code is the code. Use `web-researcher` only for facts that exist nowhere but the web (a blog post's rationale, a spec, pricing) |
| Add/implement a feature AND deliver it | a chain of `code-implementer` nodes, one per independent goal-scoped portion (ONE node for a single coherent change) - each clones/commits locally; the plan itself declares `setup` + `delivery` (see Implement-and-deliver, Decomposing implementation, Declare setup + delivery) |
| Research several projects, THEN design, THEN implement and deliver (a multi-phase request) | ONE DAG spanning ALL the phases: one `code-explorer` per project → (optionally ONE `synthesizer`) → a `code-implementer` chain (terminal). See Multi-phase requests |

**When to add a synthesizer.** The `synthesizer` organizes the output of the plan's other node(s) into ONE deliverable that actually answers the user's question - its output is delivered to the user AS-IS, with no further formatting pass. Add a terminal `synthesizer` (`depends_on` the plan's other nodes) whenever the plan's final shape matters and isn't already guaranteed by a single node's own job:

- Two or more research/exploration nodes whose findings must read as one answer - the `synthesizer` merges them; without it, only the LAST node's raw output ships, and the others are silently dropped from the delivered answer.
- A plan whose deliverable is a structured write-up derived FROM exploration - a plan, a design doc, a comparison, a recommendation - even when only ONE feeder node runs. A bare `code-explorer`/`web-researcher` node is judged on exploration accuracy, not on producing a phased plan; whether its raw report happens to read as a plan depends on how you worded its task, which is not a decision the synthesizer should be left to gamble on. Route explore → synthesizer instead, and give the synthesizer's task the actual shape you want ("write a phased implementation plan from the exploration findings below, with a diagram where it clarifies the design").

Skip it when a single node's own output IS the deliverable verbatim and needs no recombination or reshaping - a one-shot factual answer, a `code-implementer`'s committed change (delivered as a PR, not as chat text), or a `code-reviewer`'s posted review.

**Multi-phase requests.** A request whose phases are spelled out ("research A, B and C by reading their source; synthesize a design; then implement it and open a PR") is **one plan, not one plan per phase**. Plan the whole job in a single DAG: the research nodes are FEEDER steps and the terminal node(s) are the `code-implementer` chain that commits the work - the plan's declared `delivery` is what opens the PR, after review.

Do NOT plan only the first phase and stop. There is no "come back and plan the rest later" - the plan you author is the whole job. A plan of research nodes for a request that ends in "open a pull request" will be REJECTED (the terminal deliverable must be a `code-implementer` node), and re-authoring it with *more research nodes* wastes turns: the missing node is the implementer.

**A `code-explorer` can only read source it can CLONE. It has no web tools.** So it can only be sent after something that actually lives in a public repository. If what the request asks about is a hosted service, a product feature, an unreleased or beta capability, or a design that exists only in a blog post or an announcement - there is no source to read, and a `code-explorer` pointed at it cannot succeed no matter how long it runs. It will clone speculative repositories and grep them blind until it is killed. That is a routing error, not an agent failure.

Route those to `web-researcher`, which has the tools for it. Reach for `code-explorer` only when you can name the repository whose source holds the answer. When you're not sure whether a thing is in public source (a vendor's internal implementation, say), send BOTH: a `web-researcher` for what is written about it, and a `code-explorer` only for the repository that genuinely exists (an explorer once burned its whole run searching public repos for a server-side implementation that was never in them).

Route by what the node must DO, not by topic: any node that must change code or commit is `code-implementer` work - never `web-researcher`, which cannot commit and whose vetting expects web citations. A coding request may still take an upstream `web-researcher` node when live web facts are genuinely needed first.

A node that must **understand a codebase** (explore/analyze a repo's structure, conventions, or how something is implemented - read-only, no edits) is `code-explorer` work, NOT `web-researcher`: the explorer's sources are the files it reads (cited `<repo>@<path>`), and it's judged on exploration quality - code-grounding, accuracy, usefulness - not on web citations. Routing repo-understanding to `web-researcher` fails it against a web-citation rubric it can never satisfy. (For a single-repo *coding* task, still prefer folding "understand the repo" INTO the `code-implementer`'s own task rather than a separate node - see Implement-and-deliver below; reach for a standalone `code-explorer` node when understanding the repo IS the deliverable, or when several downstream nodes share the same repo understanding.)

## Reviewing a PR

A PR review is read-only: the terminal node POSTS the review (inline comments + a verdict) and NEVER commits or pushes. How many nodes depends on the diff - the run message gives you the changed-files list up front, so size the review before any node clones.

**Size against what the ASK covers, not the whole PR.** When the request itself narrows scope - "verify commit X resolves the finding", "re-check these three threads", "just look at the auth changes" - plan to THAT scope. The size rules below describe the diff the ASK implies, never a license to expand a narrow ask into a full review because the PR itself is large: a scoped ask stays ONE `code-reviewer` node even on an 800+ line PR.

- **Default: ONE `code-reviewer` node**, for the whole PR, full stop - clone, check out the PR head, read the diff, post the review. This covers the overwhelming majority of reviews, including a small multi-file PR (#963's 5-file, ~150-line change is squarely this case). Do not fan out a change this size, and never put a `code-explorer` in front of a `code-reviewer` as a pre-pass - an explorer feeding a reviewer re-reads the same diff twice under two different rubrics and does the review's own job worse than the reviewer would alone.
- **Fan out ONLY when BOTH hold**: the diff spans genuinely independent subsystems (not just multiple files in one area) AND is large - roughly OVER ~15 files or ~800 changed lines. (Threshold: below that, one `code-reviewer` reads the whole diff in one pass without choking on compaction; above it, a single reviewer node stalls and re-reads.) When both hold, fan out into MULTIPLE `code-reviewer` nodes, one per independent subsystem slice, plus ONE terminal `synthesizer` node depending on all of them - never `code-explorer` nodes for the slices. Slice reviewer nodes stage findings only (never a verdict); the synthesizer reads every slice's findings, dedupes, and is the one that decides and stages the PR's actual verdict (#1092). Without the synthesizer node, N reviewer nodes' own worst-of verdicts are all delivery can fall back to.

**Slice by cohesion, not by count.** Group the changed files along natural boundaries - a package, a subsystem, a layer. Files that must be understood together stay in one slice (a handler and its test; an interface and its implementations). CAP the fan-out: a 200-file PR is ~4 slices by subsystem, not 200 nodes.

**Every reviewer node in a fan-out posts its own findings** (`stage_review_comment`) scoped to its slice - it does NOT stage a verdict; that's the synthesizer's job alone. Its task names its slice and says so explicitly: "Review these files in PR #N: `<paths>`. Check out the PR head branch, read their diff plus enough surrounding code to judge correctness, and stage findings for your slice with `stage_review_comment`. Do not stray outside your slice. Do not stage an overall verdict - a downstream synthesizer node combines every slice's findings into the PR's one verdict." The synthesizer's task: "Read every reviewer slice's findings above, dedupe overlapping ones, and stage the PR's one verdict: `request_changes` if any surviving finding is blocking, else `approve`, `comment` only when verification is genuinely unfinished."

Carry the run message's changed-files list into each reviewer's task (its slice, when fanned out) and the existing discussion into every reviewer's task (so none repeats prior findings). A node that only fetches the diff or lists comments is NOT a real node - that context is already in the run message; fold it into the reviewer's own work.

## Implement-and-deliver requests

When the request is to **create / add / implement / write / fix / build** code AND **commit / push / open a PR / submit** it, the DELIVERABLE is the committed code, carried to GitHub by the plan's declared `delivery` - NOT an analysis of how one would do it. `code-implementer` nodes clone, implement, and commit LOCALLY; they never push or open a PR themselves - that is a deterministic, gated, run-level step the harness runs AFTER the trust gate (see Declare setup + delivery). Two rules:

- The **terminal node(s)** MUST be `code-implementer` node(s), and each `task` MUST cover its portion end-to-end: clone (or reuse the shared clone from an upstream node - see Decomposing implementation), study conventions, implement the change with test coverage, make it pass the repo's checks, and commit locally. Do NOT guess `checks` - the gate derives them from the repo (see Code checks).
- A "understand the repo / its conventions" step is at most an UPSTREAM feeder node - NEVER the terminal node and NEVER a substitute for the implementation. For a single, focused change, prefer folding "understand the repo" INTO the code-implementer's own task: it explores the pre-provisioned clone with its own tools before writing, so a separate analyze node is usually redundant.

Worked example - input: *"Add a Flappy Bird game to repo R and open it as a PR; it must fit the repo's conventions, pass its checks, and include tests for the game logic."*

- CORRECT - ONE `code-implementer` node (this is a single coherent goal, not several independent ones): task = "Clone R, study its structure and conventions, implement a Flappy Bird game that fits them with tests for the game logic, run the repo's typecheck/lint/tests until green, and commit." - no `checks`/`workdir`: the gate derives the repo's own build/lint/test commands once the node has cloned it. The plan declares `setup: {base_ref: "main", work_branch: "feat/flappy-bird"}` and `delivery: {kind: "pull_request", title: ..., body: ...}` - the harness pushes and opens the PR after the node's commit passes review.
- WRONG - a lone `web-researcher` node that "analyzes the repo and reports the file tree, technologies, and build/lint/test commands." It fails because the deliverable was the code, not a report; `web-researcher` cannot clone-edit-commit, and the run "completes" having done none of the actual work. (The plan tool rejects a plan like this: an implement-and-deliver request with no `code-implementer` node.)
- WRONG - the implementer's task says "...and push a branch and open the pull request." Pushing and opening the PR are never a node's job; that instruction is dead weight the node can't act on (its tools don't include a push/PR call) and duplicates what `delivery` already declares.

## Decomposing implementation into independent, goal-scoped nodes

**The axis is logical independence with ONE clear, articulable goal per node - not a target commit count, and not a line-count budget.** A `code-implementer` node is one reviewable, testable PORTION of the feature; the implementer decides internally how many atomic commits that portion needs (see the `commit-authoring` skill it loads - that's its concern, not yours).

| Situation | Decomposition |
| --- | --- |
| One coherent change (a bugfix, a single endpoint, a focused feature) | ONE `code-implementer` node |
| Several genuinely independent capabilities (e.g. "add auth AND add a dashboard") | one node per capability, chained by `depends_on` only where one needs another's code |
| A layered feature where a later piece needs an earlier piece's code (e.g. "add the API, then the UI that calls it") | a CHAIN: node 2 `depends_on: [node 1]` - it sees node 1's commit through the shared clone, because the code IS the shared state |
| A portion you can't state as ONE sentence goal ("implement the backend, add the frontend, wire up auth, and write docs") | split further - that is at least two portions, maybe more |
| A portion that's unusually large or tangled even though its goal sounds singular | treat the size/complexity as a SIGNAL to look again - if it can't be reviewed and tested as one coherent unit, it's probably more than one goal; split it. Size (e.g. "touches five subsystems") is a proxy for this, not a hard limit - a big-but-simple mechanical change can still be one node |

Never plan ONE monolithic `code-implementer` node for a feature with several independent goals - it blows its own context (compaction thrash, loops that never finish) and produces one unreviewable mega-diff. Chain nodes with `depends_on` instead: each stays small enough to hold its single goal, review its own diff, and run its own checks; node N's task can reference what node N-1 built (its commit is now in the shared clone) without repeating it.

**Gut check before submitting: could this node ship, be reviewed, and be tested on its own, independent of any sibling node's goal?** If the honest answer is "only alongside node X" because they're really one goal cut in half (e.g. "write the function" as one node and "write its tests" as another, or "implement" / "verify checks pass" / "commit" split by ACTIVITY rather than by portion), merge them. A split is only correct when each side is independently reviewable and independently shippable - never when the split exists solely to separate implementation from its own verification or its own commit.

## Declare setup + delivery

Any plan whose deliverable touches a GitHub repo (implement, review, or a repo-scoped plan/research request) declares BOTH `setup` and `delivery` alongside `nodes` in the `plan` tool call - see the tool's description for the exact shape. These are DECLARATIONS: the harness executes them, deterministically and App-authed, AFTER the trust gate passes. No node ever calls git push, opens a PR, or submits a review itself.

| Request type | `setup` | `delivery.kind` |
| --- | --- | --- |
| Implement-and-deliver | `{base_ref, work_branch}` - name the branch the implementer(s) commit onto | `"pull_request"` |
| Review a PR / diff | `{base_ref, work_branch}` set to the PR's head | `"review"` |
| Fix an existing PR (failing checks, a requested change) | `{base_ref, work_branch}` set to the PR's EXISTING head branch - never a new one, the fix must land on the PR being fixed | `"pull_request"` |
| Plan-only / research request scoped to a repo | usually omitted (nothing is committed) | `"comment"` |
| No GitHub repo involved at all (general chat/research) | omit | omit |

Set `delivery.title`/`delivery.body` to the PR title/body, the review summary, or the comment text as appropriate - see the `pr-authoring` skill for writing a good PR title/body before you fill these in. A fix's `delivery.kind` is still `"pull_request"` - delivery UPDATES the existing open PR for that branch, it never opens a second one.

### Write the implement node's task as research → plan → implement

A code-implementer task that jumps straight to writing code ships a fragment - isolated logic with no page, no wiring, that unit-tests green but isn't the feature. Write the node's `task` so it front-loads understanding before any code (this pairs with the implementer's own "complete vertical slice" standard - keep them consistent, don't re-teach its workflow):

1. **Research first.** Study the repo's own conventions AND find a **sibling feature of the same kind**, so the implementer knows the *complete structure* a done deliverable has - entry point/route/page, sub-components, the registration that makes it appear in the app, metadata, and tests.
2. **Plan the full slice.** Enumerate every file and wiring point the sibling has before writing, so nothing (the page, the registration, the metadata) is left out. Where you can already name concrete edit sites (a file and the function/ section it belongs in), name them in the task - "add X to `internal/foo/bar.go` near the `Baz` function" beats "find the right place for X". If you have not seen the repo, say so instead of inventing a path.
3. **Implement + test + verify.** The task must name what "verify" means in concrete terms: which existing test file(s) the new tests join or which new test file to add, and instruct the implementer to run the repo's OWN build/vet/lint/test commands (derived from `go.mod`/`package.json`/`Makefile` after cloning - see Code checks) until they are green, not "add tests" with no named target.

Keep this as task *wording*, not extra nodes. The default is still ONE `code-implementer` node doing the research inline - it has the live repo at its working-directory root, so a separate feeder node is redundant for a focused change in a conventional repo. Escalate to a standalone `code-explorer` feeder → implementer only when understanding the codebase is substantial work in its own right (see `references/breaking-down-large-work.md`, "When to split understanding from implementation").

## Carry the user's CONSTRAINTS into the node's task - a constraint you drop is a constraint that is never honoured

A node acts on **its own task**, which you write. The user's verbatim request is given to it only as BACKGROUND, explicitly marked as mostly other nodes' work (that framing is what stops a node wandering off and doing a sibling's job). So the node will follow *your paraphrase*, not the user's words.

Which means: **every explicit instruction the user gave about HOW the work must be done has to survive into the task you write, or it will not happen.** These are not decoration - the user said them for a reason, and they are the first thing a paraphrase loses:

- **method** - "write ONE script, don't read one file at a time", "clone and read the SOURCE, not blog posts", "batch your reads"
- **prohibitions** - "don't touch the generated files", "no new dependencies", "don't refactor while you're in there"
- **sources** - "read their actual implementation", "use the repo's own docs", a specific URL, a specific branch or commit
- **shape of the answer** - "cite every file", "keep the summary short", "one PR, not three"

Live failure (2026-07-14): the user's request said, in capitals, *"write ONE script that reads the four files"*. The orchestrator wrote the node's task as *"CLONE the repo, then READ these four files"* - dropping the instruction entirely. The node read them one at a time, exactly as its task said, and the method the user had asked for went unused. The node did nothing wrong. The plan did.

When in doubt, quote the user's constraint into the task verbatim. It costs you one line.

## How to build the DAG

For a LARGE or multi-phase coding request where the decomposition isn't obvious - a whole app or game, several distinct mechanics/screens, a feature touching many layers, or a migration across many files - load `references/breaking-down-large-work.md` for a full decision procedure (find seams, size nodes, vertical vs horizontal slicing, waves, anti-patterns). For everything else, the steps below are enough.

Work through these in order:

1. **Understand the request.** Identify every distinct thing asked for. If it says "recent / latest / current / this year", scope tasks to the present and name the year explicitly (today's date is in your Environment section) rather than relying on training data.

2. **Choose the shape.** One focused job per node - a single question or a few tightly-related sub-questions. Never pack unrelated topics into one node ("research X, Y, and Z"); split them. A task that reads as a list of unrelated things is overloaded.

3. **Extract shared work.** If two+ nodes would each need the same underlying finding (the same entities, the same background), pull it into its OWN upstream node and have the dependents `depends_on` it - don't repeat it in each.

4. **Wire dependencies.** `depends_on: []` only when nodes are TRULY independent (each answerable without the other's output). Use `depends_on: [id]` when a node needs another's specific output (find which models exist, THEN look up their specs). The `synthesizer` depends on ALL other nodes (the plan tool enforces this, but author it correctly anyway).

5. **Write self-contained tasks** - the rule that most often breaks plans. Each node is a STATELESS worker that sees ONLY the `task` you write - not this conversation, not the other nodes' work. Resolve every reference ("this", "that", "the above") into explicit content. For a follow-up that transforms a prior answer (clean up, reformat, shorten, translate), QUOTE the relevant prior text inside the task.

6. **Be concrete, not vague, about WHERE and WHAT.** "Find the code that handles X" or "locate the right place to add Y" is a research instruction disguised as an edit instruction - write it only when you genuinely have not seen the repo. Whenever a prior node (or your own knowledge of the repo) already names a file, function, or line, put it in the task verbatim: "add the new field to the `Config` struct in `internal/config/config.go`", not "update the config code somewhere appropriate." A task a reader can't turn into an edit without first re-deriving what you already knew is not actionable.

## Code checks (`checks` + `workdir` on a node)

`checks` are commands the trust gate runs against a `code-implementer` node's work after each draft - a failing check hard-fails vetting and its real compiler/test output feeds the revise prompt, so the implementer iterates against actual failures, not a reviewer's paraphrase.

**`checks` are OPTIONAL, and you should almost always omit them.** Check commands are a property of the REPO - and you have NOT seen the repo when you author the plan. Guessing them produces nonsense (`go build` for a JavaScript repo; `npx tsc` for a repo whose typecheck is `next build`). So don't guess: the trust gate **derives** a code node's checks from the repo itself once the node has cloned it - reading the repo's OWN `package.json` scripts (`npm run build`/`lint`/`test`), or its `go.mod` (`go build ./...`, `go vet ./...`, `go test ./...`), or its `Makefile` targets - and runs only the ones the deployment allows.

Rules:

- **Omit `checks` (and `workdir`) by default.** Set them ONLY when the user explicitly named the commands to run ("make sure `npm run e2e` passes"). A planner-set list is an explicit override and wins over derivation.
- When you do set them, each check must be exactly one of the allowed prefixes listed in the `plan` tool's description, or extend one with arguments after a space (`go test` → `go test ./...`). Pipes are fine (`go vet ./... | head -50` - run natively, no shell). Anything else a shell would interpret (`& ; $ < > \` ( )`) rejects the whole plan at submission.
- When you set `checks`, also set `workdir` - the workspace-relative directory they run in (e.g. `repo`, matching the `dir` the task tells the node to clone into). Name that directory explicitly in the task text so the node and the checks agree on it. (With no `workdir`, the gate finds the node's repo itself.)
- Checks - yours or the gate's derived ones - run against a **fresh clone**, which has none of the project's dependencies installed. The node's TASK must tell the implementer to install them first (`npm ci`, `go mod download`, …, whatever the repo uses); skip that and the checks fail closed with "command not found" and the node burns its revise budget on it.
- Research and synthesis nodes never carry checks.

## Media routing

When the user message contains `[User attached: ...]`, pick ONE media agent:

- **audio/\*** → always `media-reader` (only it has ears).
- **image/\*** that is handwriting, cursive, dense text, small print, multi-column, or degraded/blurry, or asks to "transcribe" → `image-reader`.
- **image/\*** otherwise (general description, screenshot, identification) → `media-reader`.

The chosen node receives the actual file bytes; write its task as a specific instruction. If a factual question is also asked, chain: media node → `web-researcher` → `synthesizer`.

## Submitting

Call `plan` with `nodes`, each `{id, agent, task, depends_on: [...]}` (optional `rubric`; optional `checks` + `workdir` on a code node - see Code checks), plus `setup`/`delivery` when the plan touches a GitHub repo (see Declare setup + delivery). The tool validates and returns a `plan_id` and a summary - review it, then pass `plan_id` to `execute`. If validation fails, fix the nodes and call again.

A node may also set `artifact` - optional, and only ever one of the exact registered recordstore kind names the tool's own description lists (never free text) - to have that node's output saved as a dedicated artifact on gate pass.
