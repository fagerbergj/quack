# Audit: constitution and shared prompts

- **Sources:** v0 `config/constitution.md` (247 words) and `config/prompts/`: `judge.md` (1,497), `compaction.md` (283), `compaction.summary.md` (291), `acp.environment.md` (320)
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)). Nothing was run and no model was called.
- **v1 side:** [judge requirements](../extensions/judge.md) and [RFC](../rfc/extensions/judge.md) (criteria are decision points; D15, D18), HAR-9 and RFC 0001 (pi-durable compaction), GH-3, D3, D27
- **Outcome:** drop the constitution, `acp.environment.md` and the judge prompt as files. Port three sentences of the judge prompt into the check instruction. Defer compaction to pi-durable's own and port nothing until a long session fails.

## Effectiveness

None of the five has a measurement of its own. `docs/configuration/judge-measurement.md` and the golden set grade the judge as a whole, not the constitution or any single prompt clause. The history, from commit messages:

| File | Evidence |
| --- | --- |
| constitution | Added with the trust gate (28cbeca4, June 2026) and rewritten in 33b8c2eb. It is injected as "Principles:" ahead of the rubric in the judge prompt and again in the revise prompt. No A/B with and without it. |
| judge.md | #1697: the judge prompt had a varying tool clause ahead of stable text, so judges with different tools shared only a 669-character prefix; reordered so the cache hits. #1596: judges were reading sibling nodes' pages and re-reading every cited page with no budget, fixed by scoping evidence and capping reads at 4 per round. The "a PASS with zero file reads is discarded" rule is code-enforced, not only prose. Recency leniency exists because the judge flagged real 2025 shows as hallucinations (86833b31, cited in [research.md](research.md)). |
| compaction | #1595: one research round grew from about 6k to about 70k prompt tokens; stale tool results now collapse into artifact stubs at compaction, and an earlier summary was found to be dropped on each compaction and was fixed. Those two fixes were measured by size and by tests; whether the summary prompt's quality affects later turns is not measured (none). |
| acp.environment | #1473: agents "kept re-deriving the same runtime facts (gh missing from PATH, GOFLAGS=-mod=mod failing writes in a read-only tree, stale local main) every round". The fix was to state them once; no before-and-after count is recorded. |

## Alignment and decisions

**Constitution.** Six principles: honest grounding, no fabrication, responsive and complete, internally consistent, attributable, explicit under failure. Each already has a home in a v1 check or prompt: grounding and attribution are research's `cites_fetched_sources`, `quotes_exist` and `supported_by_source`; responsiveness is `answers_every_ask`; consistency is `no_self_contradiction`; the reviewer rubric covers its own. D3 says core knows nothing about rubrics and D27 pins knowledge per agent, so a global principles file has no owner in v1. The one principle with no check is "explicit under failure" (a tool error is reported, not papered over); it belongs as one sentence in each role prompt that uses tools, and the v1 reviewer and researcher prompts already carry it (the verification-gap line, "listed under Assumptions as unverified"). **Drop the file.**

**judge.md.** The template builds one prompt from `head`, `artifact_tools`, `tail` and a read-tools or no-tools clause. Most of it describes v0 mechanics that v1 replaces:

- The "last line of defense, distrust the self-report" persona.
- The read-tools clause (about 330 words) and its rule that a zero-read PASS is discarded.
- The `artifact_tools` clause (list/read artifacts, read the referenced artifact).
- The skills clause.
- The `submit_verdict` shape with `shortfall`, `fix` and `anchor`, and "the overall score is the weakest criterion".

In v1 a check is one decision point per unit of evidence (D15, D18): the model gets the evidence and a question, answers against a schema, and the score is weighted code (`score` in `v1/packages/judge`). Evidence arrives as units, so there are no tools to describe and no zero-read rule to enforce; weakest-link scoring is replaced by weights and gates. Three ideas survive and are not covered by the check questions:

1. Do not trust the answer's own claim that it verified something. Keep as one sentence in the check instruction.
2. An unfamiliar name, title or event in web content is not evidence of error (the recency caveat above). Keep, one sentence, for checks over web-sourced text.
3. A failed check's note must name the item that failed, since it goes back to the worker verbatim as its brief. Keep, one sentence; the reviewer rubric's `feedback` function already formats the note.

The long tail clause also tells the judge "if an image is attached you can see it". The image path has no v1 consumer (see [media.md](media.md)). **Drop the file; the three sentences go into the judge's check instruction template, about 70 words against the 1,497.**

**compaction.md and compaction.summary.md.** v1 compacts through pi-durable (HAR-9, RFC 0001), keeping the originals in the log. This audit could not read pi-durable's own compaction prompt because v1's dependencies are not installed in the worktree, so whether the v0 text adds anything is not checked. Parts of the v0 text are tied to v0 mechanics: the check for a previous summary as a "model: ## Goal" line depends on how v0 renders the transcript, and #1595's artifact stubs depend on v0 artifact ids. What would carry over is the template's habit of keeping exact paths, commands, error strings and "Files & Code State" detail "because the agent will simply redo the call". That is a reasoned claim, not a measured one. **Port nothing now.** If a v1 session shows the default summary losing facts that cause repeated tool calls, test the 291-word template against the default on that session, and cut the two overlapping sentences in `compaction.md` (lines 5 and 9 both say to keep technical content and exact paths) if it ports.

**acp.environment.md.** It tells a coding agent its cwd, branch, whether the tree is read-only, how to copy it to `$TMPDIR`, that `GOFLAGS=-mod=mod` fails there, where toolchains live, `PI_ACP_STATE_DIR`, and that local `main` can be stale. v1 sessions run in a writable microVM workspace on the dev image with unrestricted shell (GH-4), so the read-only, `cp -a`, Go flag and state-dir lines are obsolete. Two facts survive: diff against the remote base ref, not a possibly stale local `main`; and CI's result already arrives with the task, so do not rerun it (v1's reviewer prompt already says this). The first goes into `github.evidence` (branch, head SHA and base ref, per the GH-3 hook). **Drop the file.**

## Conciseness

Of about 2,640 words across the five files, the port is about 70 words in the judge's check instruction and one `github.evidence` field. Nothing else is carried.

## Porting decision

1. No global constitution; per-agent prompts and per-kind rubrics carry their own principles, and the one orphan ("explicit under failure") is already in the v1 prompts.
2. Judge check instruction: the three sentences above. Nothing from the v0 judge template.
3. Compaction: use pi-durable's default; revisit only on an observed failure, with a with-and-without run.
4. `github.evidence` carries the base ref; no environment prompt file.
