# Audit: reviewer prompt and rubric

- **Agent:** `v1/agents/reviewer` (prompt v1, 733 words) and `github.reviewer` rubric v3 (`v1/packages/github/src/rubric.ts`)
- **Ported from:** v0 `extensions/github/plugin/agents/code-reviewer` (prompt 3,350 words, five 0-3 criteria) in #1830
- **Audited:** 2026-11-01, retroactively ([#1861](https://github.com/fagerbergj/quack/issues/1861))
- **Outcome:** keep both as they are. The known misses are not prompt-shaped, and the one prompt fix tried for them failed.

## Effectiveness

The evidence is a baseline replay of 22 labelled PRs (`v1-reviewer` Langfuse dataset, run `reviewer-ceb0d2ea-baseline`, local 27B), plus v1's live reviews of its own trunk.

| Measure | Result |
| --- | --- |
| Known-bad PRs blocked | 2 of 7 (#1794, #1715) |
| Known-good PRs falsely blocked | 2 of 11 (#1835, #1818); #1835 did not block on rerun |
| Median turns, wall time, input tokens | 33, 411 s, 0.8M |

The misses share a shape: the defect sits outside the diff. One example is #1816, which changed a prompt while the `matchup-recap` skill still told the model to use the cutoff the prompt removed. A one-line prompt instruction to search the repo for leftovers was replayed on #1816. It still missed, and took 70 turns and 3.3M tokens, so it was dropped. Two misses traced to the replay seeing history from after the review, and #1845 fixed that harness defect.

In live use on 2026-11-01, v1 correctly blocked:

- #1845, where the commit held only tests
- #1849, where the tests depended on the environment
- #1851, where a malformed hash crashed the page
- #1844, where a bad `--concurrency` produced an all-zero report

It made one wrong claim on #1850: that 255.255.255.255 was treated as public, when `224.0.0.0/3` already covers it. It made no false blocks on correct lane PRs.

## Alignment

- The tools the prompt names (`checkout_pr`, `submit_review`) all exist in `packages/github`, in the order the prompt gives; a passing `submit_review` posts the review itself.
- The verdict rules match the rubric's code gates:
  - Any blocking finding means `request_changes`.
  - A question or a failing CI check means `comment`.
  - A disclosed verification gap allows `comment` beside green CI.
- The labels and anchors match `parseReview` and `blocking_names_anchor`.
- The paragraph "Run something only when reading cannot settle a claim" pairs with the rubric's `toolchain_as_defect` penalty.
- The rubric covers every instruction that changes the output, apart from the plain-writing line. That line is cheap and has no check.

## Conciseness

At 733 words, the prompt is 22% of v0's. Each section maps to a rubric criterion or a tool step. The one line with no rubric counterpart is the style sentence ("Don't use just, simply or obviously"). It costs 12 words and shapes the posted text a human reads, so it stays. Cutting further would need an A/B on the dataset, and no section is a candidate on the evidence.

## Follow-ups

- Out-of-diff contradictions are the reviewer's main miss. The next attempt should be structural, giving the reviewer the files that reference the changed ones (the blast-radius pass from #1830), not another prompt line.
- The rerun disagreement on #1835 is run-to-run variance. It gets diagnosed from the two transcripts, not masked by running the review twice and voting.

## v7 delta (2026-11-04)

- **Audited:** `v1/agents/reviewer` v7 (850 words) and `github.reviewer` rubric v4, at `origin/v1` 935b32f4, against the first audit above (prompt v1 at 733 words, rubric v3).
- **Outcome so far:** prompt and rubric alignment and conciseness checked below; effectiveness is pending the v7 replay.

### What changed since v1

Prompt (733 to 850 words, +16%), three commits:

| Change | Commit | Why |
| --- | --- | --- |
| Step 4 no longer calls `post_review`; a passing `submit_review` posts the review and returns the link, and says to re-check out when the PR moved | #2039 | The reviewer posts automatically when the gate passes; the separate tool was dropped. |
| Step 1 passes the head sha and says `checkout_pr` always checks out the live head, and to stop if it is already reviewed | #2047 | From the commit title: review the live head, and end when it is already reviewed. |
| New "Answering a mention" section (about 75 words) and `reply_mention` | #2051 | `@quack` comments get an answer in their own thread. The same PR made `path:line` findings post as inline comments, which needed no prompt text because the finding format is unchanged. |

Nothing else moved: the check order, the verification paragraph, the format and the style rules are word-for-word the v1 text. The lsp cut (#2086) and the removal of `maxTurns` (#1884) touched `agent.json` and packages, not the prompt; the prompt never mentioned either.

Rubric (v3 to v4; the code started at v2 in #1830):

| Change | Commit | Why |
| --- | --- | --- |
| `quotes_exist` gate: a finding that names an identifier found nowhere in the repo, diff or dependencies fails (v3) | #1832, tightened in #1833, #2019 | The classifier judges whether a finding sounds right, not whether its quoted code exists. #2019 narrowed it to identifiers found nowhere, and #1833 made the failure list every missing quote and say fenced output is skipped. |
| `no_silent_approval` scope `change` to `approve`; v4 | #1956 | A question about an approve has no unit on any other verdict, so it scores null instead of a vacuous miss. |
| Plain-words `label` on every check | #2017 | Failed checks read in plain words. No change to any question or weight. |

`be007dbb` (#2019) and `27766d10` (#1833) changed `quotes_exist` behaviour without a version bump, so "rubric v4" covers two different gate behaviours. The baseline ran before both.

### Alignment

- **Tools.** `checkout_pr` (`checkout.ts`), `submit_review` (the judge's `toolName` in `post.ts`) and `reply_mention` (`reply.ts`) all exist and are registered for the reviewer role. Reading and running commands come from the `coding` extension in `agent.json`.
- **Every instruction has a counterpart.**
  - "Already reviewed, stop" maps to the `skipped` note in `checkout.ts` that `submit_review` refuses on.
  - "The pull request moved, check out again" matches the refusal text in `post.ts`.
  - The format and labels match `parseReview`; `path:line` findings are what `inline.ts` turns into inline comments.
  - The verdict rules match `verdict_matches_labels`, and the verification paragraph matches `toolchain_as_defect`.
- **Dead instructions.** None found. The plain-writing line still has no check (unchanged from the first audit).
- **Gap: mentions are not graded.** The "Answering a mention" section (75 words, 9% of the prompt) has no rubric criterion and no replay case, since the judge gates only `submit_review`. Its "cite `path:line`, say what you could not check" rules are unchecked.
- **Gap: `quotes_exist` has no prompt line.** The gate fails reviews for quoting missing identifiers, and the prompt says only "Every finding must rest on code you read at the path and line you cite." The gate message tells the model what to do on revision, so this costs a revision round, not a miss. Whether to add one sentence is for the replay to decide.

### Conciseness

850 words against 733 for v1 and 3,350 for v0 (25% of v0). The growth is all in the two functional changes above; no section grew without a tool behind it. The mention section is the only candidate to move out of the prompt (for example into the mention message the service already sends), which would take the prompt back to about 775 words. That needs the service to carry the instruction, so it is a proposal, not a finding.

### Effectiveness: pending the v7 replay

Not run. The audit session had no Langfuse credentials, and prod shares the GPU (the vLLM gate was at 4 running requests at the check). Run it where the Langfuse env is configured, one case at a time, only while `vllm:num_requests_running` at `http://127.0.0.1:9993/metrics` is 2 or lower:

```
docker compose run ... node packages/eval/src/main.ts run --concurrency=1 v1-reviewer reviewer-f030d3a2-v7 /config/deployment.json /app/agents/reviewer/agent.json
```

Compare against `reviewer-ceb0d2ea-baseline` in the same shape as the table above:

| Measure | Baseline | v7 |
| --- | --- | --- |
| Known-bad PRs blocked | 2 of 7 (#1794, #1715) | pending |
| Known-good PRs falsely blocked | 2 of 11 (#1835, #1818) | pending |
| Median turns, wall time, input tokens | 33, 411 s, 0.8M | pending |

Also read per case:

- **#1794 and #1715.** Were they caught again, and did any other known-bad PR flip?
- **#1835 and #1818.** Do the false blocks persist, and did `quotes_exist` newly block a good PR?
- **Revision rounds per case.** Count how many cases need a `quotes_exist` revision.
- **Turns.** Check that the mention and live-head text did not add turns.
- **Harness caveat.** The replay does not exercise `reply_mention` or auto-posting, so those changes can only be judged on alignment, not effectiveness.
