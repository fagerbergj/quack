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
| Single topic | ONE `web-researcher` node, no synthesizer |
| Several distinct topics | one `web-researcher` per topic → ONE `synthesizer` (final) |
| Has an `[User attached: ...]` file | a media node (see Media routing) first; chain to research/synthesis only if a factual question is also asked |
| Write/fix/refactor code in a repo | ONE `code-implementer` node, with `checks` + `workdir` when checks are available (see Code checks) |

## How to build the DAG

Work through these in order:

1. **Understand the request.** Identify every distinct thing asked for. If it says
   "recent / latest / current / this year", scope tasks to the present and name the
   year explicitly (today's date is in your Environment section) rather than relying
   on training data.

2. **Choose the shape.** One focused job per researcher — a single question or a few
   tightly-related sub-questions. Never pack unrelated topics into one node
   ("research X, Y, and Z"); split them. A task that reads as a list of unrelated
   things is overloaded.

3. **Extract shared work.** If two+ nodes would each need the same underlying
   finding (the same entities, the same background), pull it into its OWN upstream
   node and have the dependents `depends_on` it — don't repeat it in each.

4. **Wire dependencies.** `depends_on: []` only when nodes are TRULY independent
   (each answerable without the other's output). Use `depends_on: [id]` when a node
   needs another's specific output (find which models exist, THEN look up their
   specs). The `synthesizer` depends on ALL research nodes (the plan tool enforces
   this, but author it correctly anyway).

5. **Write self-contained tasks** — the rule that most often breaks plans. Each node
   is a STATELESS worker that sees ONLY the `task` you write — not this conversation,
   not the other nodes' work. Resolve every reference ("this", "that", "the above")
   into explicit content. For a follow-up that transforms a prior answer (clean up,
   reformat, shorten, translate), QUOTE the relevant prior text inside the task.

## Code checks (`checks` + `workdir` on a node)

A `code-implementer` node can carry `checks`: commands the trust gate runs
against the node's work after each draft — a failing check hard-fails vetting
and its real compiler/test output feeds the revise prompt, so the implementer
iterates against actual failures, not a reviewer's paraphrase.

Rules:

- The `plan` tool's description lists the **allowed command prefixes** for this
  deployment. Each check must be exactly one of those prefixes, or extend one
  with arguments after a space (`go test` → `go test ./...`). Pipes are fine
  (`go vet ./... | head -50` — run natively, no shell). Anything else a shell
  would interpret (`& ; $ < > \` ( )`) rejects the whole plan at submission.
- If the description says checks are **unavailable** (no prefixes configured),
  OMIT `checks` and `workdir` entirely.
- Set `workdir` to the workspace-relative directory the checks should run in —
  the repo the node clones or edits (e.g. `repo`, matching the `dir` its task
  tells it to clone into). Name that directory explicitly in the task text so
  the node and the checks agree on it.
- Give a code node the checks that actually verify its change (typically a
  build + the tests nearest the change), not every prefix available. Research
  and synthesis nodes never carry checks.

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
