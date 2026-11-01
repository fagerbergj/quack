# Audit: code-explorer, code-implementer and pr-tutor

- **Sources:** v0 on `main`: `extensions/github/plugin/agents/code-explorer` (prompt 629 words, rubric 1,402, `agent.yaml`, card, `memory.md`), `code-implementer` (prompt 756, rubric 2,956, `agent.yaml`, card, `memory.md`) and `agents/pr-tutor` (prompt 2,661, rubric 1,455, card 202). code-reviewer is in [reviewer.md](reviewer.md). The `memory.md` files are in [memory.md](memory.md).
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)). Nothing was run and no model was called.
- **v1 side:** [GitHub requirements](../extensions/github.md) (GH-3, GH-5, GH-6, GH-7; the roles are reviewer, implementer, responder), [GitHub RFC](../rfc/extensions/github.md), [research](../extensions/research.md) (RES-2), [judge RFC](../rfc/extensions/judge.md), D32, D33, D35
- **Outcome:** port the implementer, trimmed, with a rubric rebuilt as code gates plus two model checks. Drop the explorer as an agent and move its one reusable rule to the responder and researcher. Drop pr-tutor.

## Effectiveness

No dataset, golden item or measured outcome exists for any of the three. `testdata/judge-golden/` holds web and review items only. The explorer and implementer lived in the archived quack-extensions repo until #1515 and #1658, so `main` has just two commits touching their files after the move (`adc7be61`, `766dd601`); what is below comes from commit messages elsewhere in `main`.

| Agent | Evidence | What it shows |
| --- | --- | --- |
| code-explorer | #917 | In production the explorer never cloned third-party repos and fell back to web fetches. Three causes: a clone deny meant for the other ACP agents, `external_directory: deny`, and a prompt that described only the checked-out tree. Fixed with `acp.allow_clone` and the prompt's third-party paragraph. The paragraph exists because of a real failure. |
| code-explorer | #967, mirroring #950 | "Read first, run only when reading cannot settle a claim" was added because the explorer's report is input to another agent, which does its own running. No numbers are given. |
| code-explorer | #967 | A review plan of explorer then reviewer "reviews the same diff twice under two rubrics"; the default became one reviewer node. |
| code-implementer | #1063 | Rubric cut from 19 to 8 criteria, and the 0-2 scale widened to 0-3 because a single mild flaw on any merged sub-check sank the whole run. This is evidence about judge noise, not about whether the criteria catch bad code. |
| code-implementer | `claims_match_activity` | Its steps rest on a workspace ledger of commits and runs, "ground truth", reconstructed by code. The rubric's own design says narrated work that never happened is the failure it is for. No count of how often it fired is recorded. |
| all three | none | No benchmark compares any of these prompts with a shorter one. |
| pr-tutor | f9ab229a (#1572), 46996bb2 (#1599), 766dd601 (#1784) | The spike's answer key was the longest option in 23 of 26 questions, which is why the prompt demands equal-length distractors. QA on #1553 found raw backticks in labels, a reply that narrated its revisions, and a round-1 judge that ran out of output tokens. Over 30 days in prod the `mermaid_valid` criterion never failed and `check_mermaid` caught one invalid diagram (#1784). The repo holds no count of how often pr-tutor was run or whether anyone read a quiz. |

## code-explorer

**Alignment.** v1 has three GitHub roles (reviewer, implementer, responder; G4, GH-5) and no explorer. Its jobs split two ways:

- Understanding the checked-out repo is the responder's job ("answer questions in PR and issue threads") and is what the lifecycle hook's `github.evidence` already holds (conventions, blast radius; GH-3).
- Reading a third-party repo is research: RES-2 lists "code repositories" as a source. The research audit already notes the gap: a cloned file has no `[sN]` id, so `cites_fetched_sources` would fail a claim taken from a repo. That is a spec gap to settle in research, not a reason to keep an agent.

Prompt, by instruction:

| Instruction | Decision |
| --- | --- |
| Read-only, never modify, never push | Drop as a rule. The responder's only gated write is `github_comment` (GH-6), so the prompt need not forbid edits. |
| Third-party repos: shallow clone into `$TMPDIR`, cite `<repo>@<path>`, never research it from the web | Keep for research, one sentence. Needs the egress allowlist for the clone host (D25) and the citation fix above. Evidence: #917. |
| "Ground every claim in a file you read this session; never assert from a filename or training" | Keep, in the responder and researcher prompts. |
| Read first; run only when reading cannot settle a claim, and say you ran something | Already in v1's reviewer prompt; add to the responder. |
| Orient at README, AGENTS.md, build files; "find by shape, then read" | Drop. GH-3 hands the conventions over; "find by shape" is what the model does anyway (untested). |
| Read-only tree, `$TMPDIR`, `go test -overlay`, `cp -a` | Drop. v1 sessions get a writable workspace in a microVM (GH-4). |
| "Once you can accurately answer, write it" | Keep as a stop rule only if a responder run shows over-exploration; no evidence. |

Rubric: `accurate`, `answers_the_task`, `clear_and_actionable`, `grounded_in_code`.

- `grounded_in_code` ports as a code gate: every `path` the reply cites was opened by a read call in the session log. v1's log makes that exact; v0 needed a ledger and a judge. This is the only criterion with a mechanical form.
- `accurate` ports as one model check scoped to each load-bearing claim, given the cited file window.
- `answers_the_task` keeps its negative-result rule ("a well-evidenced NEGATIVE result answers the task": name what was searched, say what was found, say where the answer likely lives). Port that as one sentence of the responder prompt. I found no commit message in `main` explaining the rule's origin, so there is no evidence beyond the rule itself.
- `clear_and_actionable` mixes path precision (already inside `grounded_in_code`) with a no-preamble rule; drop.

**Decision.** No explorer agent. The responder's prompt takes the read-first rule, the cite-what-you-read rule and the negative-result rule (about 90 words). Research takes the third-party clone paragraph once its citation question is settled. The code gate comes from `grounded_in_code`.

## code-implementer

**Alignment with GH-5 and GH-6.** v0's delivery model is "commit locally, call `stage_pr` or `stage_push`, the gate pushes". In v1 the agent itself calls the gated actions `github_open_pr` and `git push`, each with a sealed `github.pr_body` artifact, and the judge checks at the gate. So the whole "you never push" and "call whichever tool this round actually offers by its exact name" paragraphs (about 190 words) describe a v0 mechanism and go.

Prompt, by instruction:

| Instruction | Decision |
| --- | --- |
| Commit atomically, Conventional Commits, one concern each, builds green at each | Keep. |
| Done means committed; report only what happened; an honest "I could not complete X" passes | Keep, in one sentence. The log is ground truth in v1, and the checks below read it. |
| Follow the repo's AGENTS.md; do not edit generated files | Keep. GH-3 delivers the conventions; the generated-file warning is cheap and evidenced by this repo's own rule. |
| Read AGENTS.md and install dependencies first | Keep "install dependencies" (a fresh clone has none). Reading AGENTS.md is done by the hook. |
| Complete vertical slice: find a sibling feature and match its entry point, wiring, metadata and tests | Keep. It targets the usual failure (green unit tests over unwired logic) and is the one instruction that names a structural check. Untested. |
| Load ponytail, run ponytail-review on your diff, load develop-feature or fix-bug, use pr-authoring | Decide on the benchmark. Four skill loads per run, justified in the prompt as "the judge scores against ponytail", i.e. teaching to the rubric. No run compares with and without. Keep pr-authoring (it shapes the one artifact the operator reads) and drop the other three until a benchmark shows they help. |
| After `stage_pr`/`stage_push`, reply with one short line | Drop; v1's artifact is the PR body. |
| Review feedback sends you back: address exactly what it names | Keep; it matches the yield hook's feedback loop (JDG-1, JDG-4). |
| `Worth remembering:` lines | Drop; see [memory.md](memory.md). |

Rubric, 8 criteria. The v0 rubric has no check on whether the code builds: `checks_pass` is a separate deterministic criterion outside the file (named in its notes). v1 keeps that as CI and test results.

| v0 criterion | Decision |
| --- | --- |
| `claims_match_activity` | Port as a code gate. The log lists the commits and command runs; a claimed SHA or test pass absent from it fails. No judge. |
| `complexity_proportionate` (five merged sub-checks) | Split. Cyclomatic complexity, duplication and comment length are already code gates in this repo (`tools/sloplint`, golangci-lint `dupl`, comment runs of at most 2 lines); run them on the diff. Diff minimality and YAGNI stay as one model check: "does every hunk serve the stated task?" (`minimal_diff`). |
| `module_shape`, `coupling` | Merge into `minimal_diff`'s instruction or drop. #1063 merged them under the owner's design decisions; neither has evidence of catching a defect. Drop unless the benchmark shows a miss. |
| `naming_and_comments` | Drop; comment runs are a code gate and there is no record of naming findings changing an outcome. |
| `error_handling` | Drop. One narrow rule ("do not wrap a trivially recoverable case in boilerplate") with no evidence of use. |
| `commit_hygiene` | Port in two parts: a code check that the subject matches the Conventional Commits pattern and the branch is not a placeholder (`fix`, `update`, `wip`); one model check for "files touched that the task never named" with that list computed in code, as v0 already did. |
| `task_completeness` | Drop. Half of it checked that the agent did not claim a push it could not do, which does not apply when it can; the other half is covered by `minimal_diff` and the issue's own acceptance text. |

Resulting rubric: gates `claims_match_activity`, `checks_pass`, `sloplint_clean`, `commit_message_form`; weighted model checks `minimal_diff` and `unnamed_files_justified`. This is a proposal built from the v0 structure, not a measured improvement; per EVL-5 it needs at least 20 real cases including known-bad ones before it gates anything, and none exist for the implementer.

**Conciseness.** The prompt goes from 756 to about 430 words: the delivery paragraphs (~190), the post-stage reply rule (~80) and three skill loads (~40) are the cuts. The rubric goes from 2,956 words and 8 criteria to about 6 checks, four of them code.

**Decision.** Port as `implementer` for the GitHub extension with the trimmed prompt and the rubric above. Drop the card (routing text), `agent.yaml`'s `judge_rounds: 1` becomes the manifest's `maxRevisions` (J1, start at 1 as in v0), and `memory.md`.

## pr-tutor

**What it is.** A native v0 agent that reads a PR and renders one interactive surface through `render_ui`: overview, layered Diagram, Risk table, key hunks and a 3-5 question quiz graded server-side against a stored answer key. The prompt is the largest in the repo set: 2,661 words with a 1,400-word worked example, and a promptlint override of 5,000 tokens.

**Alignment.** v1 has no A2UI, no `render_ui` and no quiz or grading. Its GitHub scope is four jobs (G4), none of them teaching; artifacts are finished readable documents with versions and check status (UI-3, D42), not interactive surfaces with state. Its parts map to v1 as follows:

- Overview and key changes: what a `github.reply` from the responder already is.
- Risk table with blast radius: GH-3 computes the blast radius from language servers, which is better than pr-tutor's own "diff-only, never claim a caller you did not see". The reviewer and implementer already receive it.
- Layered flow diagram: no v1 consumer; the diagram schema has validators and caps that are v0 UI code.
- Quiz: no requirement, no trace to a goal in the PRD, and no evidence anyone used it. The repo records neither usage nor a decision to build it for a goal.

The prompt's reusable rules are about quiz writing: distractors as long and specific as the answer, options differing in substance, the key written from the `why` before choosing the option letter, and never altering a graded question.

**Decision: drop.** No v1 slice owns it, its output format is a v0 surface type v1 does not have, and there is no measure of use. If the operator later wants a PR explainer, the owning slice is GitHub: a responder run triggered by a comment command, writing a `github.reply`, with a quiz treated as a new requirement and a new benchmark rather than a port. Keep the four quiz-writing rules above in this file as the only salvage.

## Follow-ups

- The citation gap for repository claims in research (above) decides whether third-party repo reading ports at all.
- The implementer rubric needs a labelled set before the checks above gate a push.
- Whether the three skill loads help is one A/B on that set.
