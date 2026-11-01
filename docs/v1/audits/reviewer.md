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

- The tools the prompt names (`checkout_pr`, `submit_review`, `post_review`) all exist in `packages/github`, in the order the prompt gives.
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
