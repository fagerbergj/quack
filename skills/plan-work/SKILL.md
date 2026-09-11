---
name: plan-work
description: >
  How to turn a request into assignments for create_plan / edit_plan / execute:
  which agent does what, how to decompose, how to write a task a node can act
  on. Load before authoring a plan.
---

# Plan Work

A plan is a list of **assignments**: each ties a node (a person doing one agent's job) to a `task`, with `depends_on` naming the assignments whose results it receives. `create_plan` starts one, `edit_plan` changes it, `execute` runs whatever has not run yet and returns each node's result.

Every assignment is either `{agent, task, depends_on}` - hires a new node, `agent` being an exact name from the **Agents** list in your system prompt - or `{node_id, task, depends_on}`, reassigning a node `list_nodes` showed you, which continues its own session. One of `agent` and `node_id` is required on every assignment, including the second and third. A sibling being hired in the same call has no id yet, so name its 0-based position in this `assignments` array instead of inventing one.

**Before every execute, ask: if this runs as written, does the user get back what they asked to receive?** Name the artifact the request wants and find the assignment whose own task produces it. Exploring, investigating or "producing a report" never satisfies a request for a plan, a review or shipped code; it can only feed the assignment that produces the real thing.

## Plan in steps when you cannot see the whole job yet

Until the plan declares `delivery`, `execute` is a **step**: it runs the new assignments, returns each one's status and result preview in the same turn, and you continue. Use that when a later assignment depends on what an earlier one finds (which file, which function, whether the thing exists at all): run the explorer, read its result, then `edit_plan` to add the implementer with a task that names what was found and `depends_on` the explorer, and `execute` again. Once `delivery` is declared, `execute` is terminal: the answer goes to the user directly and you write nothing after it.

When the shape is already clear, declare the whole plan in one call. Do not run steps to postpone thinking.

## Common workflows

| Request | Assignments |
| --- | --- |
| One information topic | one `web-researcher` |
| Several distinct topics | one `web-researcher` each, then one `synthesizer` depending on all |
| `[User attached: ...]` file | a media node first (see Media); chain to research only if a factual question is also asked |
| Change code in a repo | one `code-implementer` |
| Review a PR / diff / branch | one `code-reviewer`; fan out only when the diff is both multi-subsystem and large (see Reviewing) |
| Explain a codebase, its conventions, how X is implemented here | one `code-explorer` (clones and reads, cites files, never commits) |
| How does ANOTHER project implement X | one `code-explorer` per project: it clones their repo. Not `web-researcher`; articles describe code, only code is code |
| Produce an implementation plan (not the code) | `code-explorer` / `web-researcher` feeders, then a `synthesizer` that writes the plan |
| Implement and deliver a feature | one `code-implementer` per independent portion, chained by `depends_on` where one needs another's code; plan declares `delivery` |
| Research, then design, then implement | one plan across all phases, or explorer steps first and the implementer added once you know the edit sites |

**Synthesizer.** Its output is delivered verbatim, so add one whenever the final shape matters and no single node's job guarantees it: two or more feeders whose findings must read as one answer (otherwise only the last node's output ships), or a write-up derived from exploration (a plan, a design, a comparison). Skip it when one node's output is the deliverable: a factual answer, a committed change, a posted review.

**A `code-explorer` can only read what it can clone.** A hosted service, an unreleased feature, a design that lives in a blog post has no source to read; send `web-researcher`. Unsure whether the source is public: send both, the explorer only for a repository you can name.

## Reviewing a PR

Read-only: the reviewer posts findings and a verdict, never commits. Size to what the ask covers, not the PR: "re-check these three threads" is one `code-reviewer` on an 800-line PR.

- Default is one `code-reviewer` for the whole diff. Never put a `code-explorer` in front of it; that reads the same diff twice under two rubrics.
- Fan out only when the diff spans genuinely independent subsystems AND is large (over about 15 files or 800 changed lines): one `code-reviewer` per subsystem slice, each staging findings for its slice only with `stage_review_comment` (its task says: Do not stage an overall verdict), plus one `synthesizer` depending on all of them that dedupes and stages the single verdict. Slice by cohesion (a package, a layer), not by file count.
- Carry the changed-files list and the existing discussion into each reviewer's task so none repeats prior findings. A node that only fetches the diff or lists comments is not a node; that context is already in the run message.

## Implement and deliver

The deliverable is committed code carried to GitHub by the plan's `delivery`. `code-implementer` nodes clone, implement, and commit locally; the harness pushes and opens or updates the PR after the trust gate. The terminal assignment is a `code-implementer` whose task covers its portion end to end: study conventions, implement with tests, run the repo's own checks until green, commit. Never tell a node to push or open a PR; it has no tool for it.

Fold "understand the repo" into the implementer's task for a focused change; it explores the provisioned clone before writing. Use a separate `code-explorer` only when understanding is substantial work on its own or several nodes need the same understanding.

**Decompose by independent portion with one articulable goal, never by activity.** One coherent change is one node, even though that node writes its own tests and runs its own checks. Several independent capabilities are one node each, chained only where one needs another's code (node N sees node N-1's commit through the shared clone). A portion you cannot state in one sentence is two portions. A monolithic node for several goals blows its context and yields one unreviewable diff. For a whole app, several mechanics, or a migration across many files, load `references/breaking-down-large-work.md`.

## Writing a task

The node sees only its `task` text plus the results of what it `depends_on`, never this conversation.

1. **Self-contained.** Resolve every "this", "that", "the above" into content. A follow-up that transforms a prior answer quotes the relevant text.
2. **Concrete about where and what.** When you or an earlier step already know the file, function, or line, put it in verbatim: "add the field to `Config` in `internal/config/config.go`", not "update the config code". "Find the right place" is only honest when nobody has seen the repo yet.
3. **Research, plan, implement.** An implementer task front-loads understanding: find a sibling feature of the same kind, enumerate every file and wiring point it has, then implement, test, and run the repo's own build, lint and test commands until green. Name the test file the new tests join.
4. **Carry the user's constraints verbatim.** Method ("write ONE script"), prohibitions ("no new dependencies"), sources ("read their actual implementation"), answer shape ("cite every file"). A paraphrase loses them first, and the node will follow your paraphrase, not the user's words.
5. **Scope to the present** when the request says recent, latest, or current; the date is in your Environment section.

## Setup, delivery, checks

| Request | `setup` | `delivery.kind` |
| --- | --- | --- |
| Implement and deliver | `{repo, base_ref, work_branch}` | `pull_request` |
| Review a PR | the PR's head | `review` |
| Fix an existing PR | the PR's existing head branch, never a new one | `pull_request` (updates that PR) |
| Plan or research scoped to a repo | omit | `comment` |
| No repository involved | omit | omit |

`delivery` carries only `kind`; the PR title and body are written by the implementer node (its `stage_pr` call, per the `pr-authoring` skill), so put any wording requirements in that node's task. On a GitHub-triggered chat the run clones the trigger's repo and base ref and ignores a submitted `setup`; declare only `delivery`.

`checks` and `workdir` on an assignment are optional and almost always omitted: you have not seen the repo, and the gate derives a code node's checks from its `package.json`, `go.mod`, or `Makefile` after the clone. Set them only when the user named the commands. Checks run in a fresh clone, so the task must tell the implementer to install dependencies first.

An agent's artifact kind is declared on its own bundle, never per assignment.

## Media

`[User attached: ...]`: audio goes to `media-reader`; an image that is handwriting, dense text, small print, or degraded, or asks to transcribe, goes to `image-reader`; any other image goes to `media-reader`. The node receives the bytes; write its task as a specific instruction.
