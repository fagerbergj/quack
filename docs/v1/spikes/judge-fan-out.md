# Judge fan-out spike

Question: does judging chunks of an artifact (fan-out) and combining the chunk answers by declared rules (fan-in) make grading faster and more decidable than today's whole-artifact checks, on real research reports and PR reviews?

Short answer: it is much more decidable and faster for the checks whose input splits into items (promises, contract guarantees and parts, summary sentences, fact pairs), and it is not a free win for `answers_every_ask`, where the per-ask question was worse than the whole question on the labelled set. Deterministic splitters are enough for structured input (diff, OpenAPI, JSON Schema, Markdown sections, bullets) and fail on free-form prose (requests, PR descriptions), where a model splitter with span verification found every labelled item.

All code is under `v1/tools/spike-fanout/`; nothing in `packages/` or any production rubric changed.

## What was built

A strategy library (`strategies.ts`) and six-plus candidates written only as declarations over it (`declarations.ts`). There is no per-candidate chunking, aggregation or glue code.

- **Splitters** (`split` on a scope): `markdownSections`, `listItems`, `sentences` (optional clause `splitOn`, `match`, `skip`, `max`), `diffFiles` (optional `filter: added`), `diffHunks`, `jsonItems`, `openapiOperations`, `schemaProperties`, `dtsExports`, `relevanceWindow`, `digestPairs` (a digest extractor plus a comparator: the map and compare halves of map-reduce) and `llm`. A deterministic splitter may declare `expect` (`min`, `max`, `maxChars`) and a `fallback`; the splitter that ran, and why a fallback fired, is recorded per check.
- **Context selectors** (per-chunk extra state): `whole`, `sections`, `items` and `related`, which pulls the chunks of another input that match this one by `names` (the other chunk's file is named in the piece), `cites` (the piece's file is named in the other chunk), `overlap` (shared words) or `sibling` (same directory and name suffix, newest first). `match` takes a list, tried in order.
- **Aggregators** (`aggregate` on a check): `all`, `any`, `share`, `atLeast`, `coverageUnion` (a chunk is covered when code found related material, when the model says so, or either). They fold three-valued answers: a chunk answer at or above 0.8 is true, at or below 0.2 false, else unknown, and a check is decided only when no assignment of the unknown chunks can change the result. That is the "decided rate" below. Map-reduce is `digestPairs` followed by `any` over the pairs.
- **Declaration shape**: a scope is `{ split, context, piece }`; a check is `{ scope, question, aggregate, flagWhen }`. Two checks on one scope are sent to Clef as one request per chunk, as the judge does per unit. `wire` lets variant checks share a question so one Clef answer serves them.
- **Tests**: `strategies.test.ts` pins the splitters, the three-valued folds, span verification, the sweep and the fallback.

Each candidate below shows its declaration. Every one uses only library strategies; nothing was dropped for needing custom code. Three strategies were added while building (named so they can be kept): `digestPairs` + `numbers` extractor + `sameTermsOtherValue` comparator (candidate 3), `related` with a `sibling` matcher (candidate 5), and `llm` with `sweep` (the splitter work).

## Method and data

- **Clef** was called directly from the server through an ssh pipe into `quack-v1` (`bridge.js`, a few lines of Node sent on stdin, nothing written to prod), one request at a time, as the `noul`/`choice` wire shapes of `system-one-shared.js`. Latency is measured inside the container. The 27B was never called. About 600 Clef requests were sent in total, over the "few hundred" guideline: roughly 100 were wasted re-asking cached states because a question id is part of the request body (fixed with `wire`), and the owner's added scope (contracts and splitters) came after the first 350.
- **Research:** the 20 labelled cases of `tools/research/cases.ts`; flag rule as in `measure.ts` (a positive check is flagged below 1, a pitfall above 0). Catch is over the cases whose `targets` name the check, false block over the 5 good cases, so each rate rests on 2 to 5 cases. "Best guess" resolves an unknown chunk by its probability above 0.5; "decided only" drops cases the fold left undecided. Whole-artifact rows send today's question verbatim as one request (40 requests), so the comparison is Clef against Clef on the same cases. 17 real `research.report` artifacts on prod were chunked; Clef ran on four of them. They have no labels.
- **Reviews:** 14 of the 217 recorded `github.review` artifacts (#2186, #2184, #2183, #2179, #2171, #2170, #2165, #2161, #2149, #2142, #2153, #2143, #2182, #2173; rubric `github.reviewer@4`, all with an approve verdict), picked for diff size and, for sibling_parts, for adding a file. The 27B's recorded answer is read from the verdict's per-check `resolvers` (`promises_handled` escalated on 13 of the 14, `sibling_parts` on all 14). Diffs and descriptions come from `gh`; the function-context diff is `git diff --function-context` from the PR's merge base. No PR in the set linked an issue.
- **Contracts:** four of the eight recorded `contract.spec` artifacts (`5be69037`, `b20d0f36`, `608df011`, `dfb279cf`: three REST, one JSON Schema). All eight passed in prod with the 27B answering all three model checks, so agreement means "also finds nothing wrong", not "catches a defect".
- **Today's path** per check is one Clef call (median about 0.4 s) plus 25 s per serial 27B escalation. For research the whole question was actually asked, so "today" is real timing plus 25 s when the Clef answer was undecided. For reviews and contracts the recorded resolvers say the 27B answered, so today is 0.4 + 25 s.
- **Unit budget:** tokens are estimated as JSON length over 4, the judge's own rule, against the 8,192 cap.

## Findings by candidate

### 1. `answers_every_ask`

Declaration:

```ts
scopes.asks = { split: { name: 'sentences', from: 'request', splitOn: ASK_SPLIT, match: ASK_LIKE }, piece: 'ask',
  context: { request: whole(800), summary: sections(Summary), findings: related(listItems Findings, overlap, top 6), assumptions: sections(Assumptions) } };
checks.answers_every_ask = { scope: 'asks', question: bool('Is this ask answered in the Summary or Findings, or named under Assumptions as unsettled?'), aggregate: { name: 'all' }, flagWhen: false };
```

It uses `sentences`, `whole`, `sections`, `related` and `all` only. Coverage union is not used: an ask is a covered/uncovered question the model answers, with the findings retrieved by code as context.

**answers_every_ask**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 2 / 3 |
| tokens per chunk (median / max, cap 8192) | 178 / 224 |
| oversize chunks (escalate unasked) | 0 of 42 |
| Clef latency per chunk (median / max, s) | 0.3 / 0.5 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 98% (41/42) |
| artifacts whose check is decided by the fold | 95% (19/20) |
| Clef time per artifact (median / max, s) | 0.6 / 0.9 |
| Clef plus 27B for what stays undecided (median / max, s) | 0.6 / 25.5 |
| today, whole artifact (median / max, s) | 0.3 / 25.4; whole-question Clef answer decided on 95% (19/20) of cases |
| labels, fan-out | catch on seeded-bad 25% (1/4), false block on good 0% (0/5); decided only: catch 33% (1/3), false block 0% (0/5) |
| labels, whole question to Clef | catch on seeded-bad 100% (4/4), false block on good 0% (0/5); decided only: catch 100% (4/4), false block 0% (0/5) |

Notes. On the 20 labelled cases the splitter finds every ask (42 of 42) with no model. Fan-out decides 98% of chunks but gets the answer wrong: it said "answered" for `when does the work start` with no start date anywhere in the report, because the chunk shows only an unrelated Summary and `(none)` for related findings. Clef's whole-question answer caught all four unanswered-ask cases; the per-ask question caught one. The ask came with nothing to ground an answer and Clef still said yes. Requiring the code's retrieval to have found something would catch it, but the overlap retrieval returns nothing for `when does the work start` on good reports too (`good-harbour`), so that rule would false-block; it was not tried.

On real reports the whole `{request, report}` state is over the 8,192-token cap on 4 of 17 reports (they go straight to the 27B); every ask chunk fits, because findings are retrieved, not pasted.

Call: **needs a different question.** Fan-out removes the cap problem and keeps time near 1 s, but on this set it lost 3 catches the whole question made. The set is 4 seeded-bad cases, so this is a signal, not a rate.

### 2. `summary_follows_findings`

Declaration:

```ts
scopes.summarySentences = { split: { name: 'sentences', from: 'report', sections: ['Summary'], strip: CITE }, piece: 'statement',
  context: { findings: related(listItems Findings, overlap, top 8) } };
checks.summary_follows_findings = { scope: 'summarySentences', question: bool('Does the statement follow from the findings?'), aggregate: { name: 'all' }, flagWhen: false };
```

The spike asks "supported" and folds with `all`, which is the same as `any` unsupported without a negative-polarity question.

**summary_follows_findings**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 1 / 1 |
| tokens per chunk (median / max, cap 8192) | 153 / 192 |
| oversize chunks (escalate unasked) | 0 of 20 |
| Clef latency per chunk (median / max, s) | 0.3 / 0.3 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 95% (19/20) |
| artifacts whose check is decided by the fold | 95% (19/20) |
| Clef time per artifact (median / max, s) | 0.3 / 0.3 |
| Clef plus 27B for what stays undecided (median / max, s) | 0.3 / 25.3 |
| today, whole artifact (median / max, s) | 0.4 / 25.4; whole-question Clef answer decided on 90% (18/20) of cases |
| labels, fan-out | catch on seeded-bad 100% (2/2), false block on good 20% (1/5); decided only: catch 100% (2/2), false block 20% (1/5) |
| labels, whole question to Clef | catch on seeded-bad 0% (0/2), false block on good 0% (0/5); decided only: catch 0% (0/1), false block 0% (0/5) |

Notes. Research summaries are one or two sentences, so the median artifact has one chunk and fan-out is one smaller request. The gain is accuracy: the whole-report question missed both overreach cases (catch 0 of 2; it answered 0.96 and 0.76), the per-sentence question caught both. It false-blocks `good-grant` (1 of 5), against 33% for the 27B in the audit; one case is not a rate.

Call: **keep as fan-out.** The per-sentence question is the better question; the gate to move off observe-only is the audit's 10% false block on a larger good set.

### 3. `no_self_contradiction`

Declaration:

```ts
scopes.factPairs = { split: { name: 'digestPairs', of: { name: 'sentences', from: 'report', strip: CITE }, extract: 'numbers', compare: 'sameTermsOtherValue', minShared: 2, top: 3 } };
checks.no_self_contradiction = { scope: 'factPairs', question: bool('Do the two passages state conflicting values for the same thing?'), aggregate: { name: 'any' }, flagWhen: true };
```

The extractor is deterministic (money, times, dates, numbers with a unit; the passage's other words are its terms), and so is the comparator (two passages conflict when, in some fact class both have, neither value set contains the other and they share two terms). No model is used to extract. A report with no such pair is decided in code with no Clef call.

**no_self_contradiction**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 1 / 3 |
| tokens per chunk (median / max, cap 8192) | 129 / 144 |
| oversize chunks (escalate unasked) | 0 of 17 |
| Clef latency per chunk (median / max, s) | 0.2 / 0.2 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 100% (17/17) |
| artifacts whose check is decided by the fold | 100% (20/20) |
| Clef time per artifact (median / max, s) | 0.2 / 0.7 |
| Clef plus 27B for what stays undecided (median / max, s) | 0.2 / 0.7 |
| today, whole artifact (median / max, s) | 0.4 / 25.4; whole-question Clef answer decided on 95% (19/20) of cases |
| labels, fan-out | catch on seeded-bad 100% (2/2), false block on good 0% (0/5); decided only: catch 100% (2/2), false block 0% (0/5) |
| labels, whole question to Clef | catch on seeded-bad 0% (0/2), false block on good 100% (5/5); decided only: catch 0% (0/2), false block 100% (5/5) |

Notes. 15 of 20 cases had a candidate pair; the other 5 were decided without Clef. Both seeded contradictions were found and no good report false-blocked. The two seeded flaws are single numbers in a sentence, which is exactly what the extractor handles; a contradiction in names or prose would not produce a pair, so recall on realistic contradictions is untested.

The surprise is the whole-report row. With today's question verbatim, Clef answered 0.86 to 0.93 on 17 of the 18 reports without a conflict (0.68 on the other) and 0.06 and 0.12 on the two with one. As a pitfall question (`Does any figure, date or name conflict?`) that is inverted: it would flag all 5 good reports and none of the seeded ones. The pair question, worded the same way round, was answered the right way on every pair. It is one wording and 20 reports, so it is a lead for the whole-report question, not a finding about Clef.

Call: **keep as fan-out (map-reduce).** It is cheap, decided, and the whole-report version of the question is not usable on this evidence.

### 4. `promises_handled`

Declaration:

```ts
scopes.promises = { split: { name: 'sentences', from: ['description', 'issues'], skip: [...], match: PROMISE_VERBS, minWords: 5, max: 5 }, piece: 'promise',
  context: { hunks: related(diffHunks of the function-context diff, match [names, overlap], top 3), review: sections(Takeaway, Findings) } };
checks.promises_handled = { scope: 'promises', question: choice(not_a_promise | delivered | deferred | reported | missing), aggregate: { name: 'all', pass: [not_a_promise, delivered, deferred, reported] }, flagWhen: false };
```

Hunk menus (Clef picking hunks) were not built; the `related` selector falls back from file names to word overlap, which is deterministic and free.

**promises_handled**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 5 / 5 |
| tokens per chunk (median / max, cap 8192) | 2212 / 2666 |
| oversize chunks (escalate unasked) | 0 of 58 |
| Clef latency per chunk (median / max, s) | 2.3 / 4.6 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 95% (55/58) |
| artifacts whose check is decided by the fold | 86% (12/14) |
| Clef time per artifact (median / max, s) | 8.1 / 16.0 |
| Clef plus 27B for what stays undecided (median / max, s) | 8.1 / 62.9 |
| today, whole artifact (median / max, s) | 25.4 / 25.4; recorded: escalated to the 27B on 13 of 14 |
| agreement with the recorded 27B answer | 100% (13/13); where the fold decided: 100% (11/11) |

Notes. Fan-out turns a check that escalated on 13 of 14 reviews into one the fold decides on 12 of 14, and the Clef time is 8 s median against about 25 s for one serial escalation. Chunks are 2,200 tokens, well under the cap. The two undecided reviews (#2153, #2143) each add a file; I did not look into why. The whole `{review, description, diff}` state is over the 8,192-token cap on 10 of the 43 fetched reviews, which go straight to the 27B today; no promise chunk was over it.

The agreement figure is weak evidence: the 27B recorded "all delivered" on all 14 reviews, so the fan-out agrees by also finding nothing missing. It shows no false block; it does not show the fan-out would catch a missing promise. The deterministic splitter's cap-of-5 and delivery-verb filter picked sentences of the bug narrative over the fix sentences on three of five reviews (see the splitter section), so recall of real promises was 59% until the model splitter replaced it.

Call: **keep as fan-out, with the model splitter on free-form descriptions.**

### 5. `sibling_parts`

Declaration:

```ts
scopes.newFiles = { split: { name: 'diffFiles', from: 'diff', filter: 'added' }, piece: 'newFile',
  context: { sibling: related(jsonItems repoFiles, match sibling, top 1, require: true) } };
checks.sibling_parts = { scope: 'newFiles', question: bool('Does the sibling file have a part the new file lacks, apart from what belongs only to the new file's own purpose?'), aggregate: { name: 'any' }, flagWhen: true };
```

**sibling_parts (triggered on 2 of 14 reviews)**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 1 / 1 |
| tokens per chunk (median / max, cap 8192) | 810 / 1630 |
| oversize chunks (escalate unasked) | 0 of 2 |
| Clef latency per chunk (median / max, s) | 0.9 / 1.9 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 50% (1/2) |
| artifacts whose check is decided by the fold | 50% (1/2) |
| Clef time per artifact (median / max, s) | 0.9 / 1.9 |
| Clef plus 27B for what stays undecided (median / max, s) | 1.9 / 25.9 |
| today, whole artifact (median / max, s) | 25.4 / 25.4; recorded: escalated to the 27B on 14 of 14 |
| agreement with the recorded 27B answer | 50% (1/2); where the fold decided: 0% (0/1) |

Notes. The trigger the owner described (a new file whose directory holds a same-suffix sibling) fired on 2 of 14 reviews, 2 of the 8 reviews that add any file. `require: true` means a review with no such pair has no unit and the check scores null, which is the right answer. With two units there is no rate: Clef was undecided on one and on the other said no gap while the 27B recorded one (#2182: the review "never compares the new issueBase setting to its sibling"). The 27B recorded a sibling gap on 5 of the 14 reviews, though only 2 add a file with a same-suffix sibling: its question is broader than this trigger.

Call: **needs a different question.** The chunking works and is nearly free in code; the pair question ("lacks a part") is an open-ended difference and Clef did not decide it. Do not weight it until there are 20 or more triggers.

### 6. Review coverage (measurement only)

Declaration:

```ts
scopes.files = { split: { name: 'diffFiles', from: 'diffW' }, piece: 'diff',
  context: { reviewLines: related(listItems of Findings/Verified/Notes, match cites, top 6) } };
checks.covered = { scope: 'files', question: bool('Do the review lines shown say something specific about this file?'), aggregate: { name: 'coverageUnion', by: 'either', min: 1 }, flagWhen: false };
checks.unnamed_defect = { scope: 'files', question: bool('Does this diff contain a defect that the review lines shown do not name?'), aggregate: { name: 'any' }, flagWhen: true };
```

**Per-file review coverage (measurement only)**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 3 / 10 |
| tokens per chunk (median / max, cap 8192) | 817 / 4851 |
| oversize chunks (escalate unasked) | 0 of 63 |
| Clef latency per chunk (median / max, s) | 1.1 / 11.6 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 97% (61/63) |
| artifacts whose check is decided by the fold | 100% (14/14) |
| Clef time per artifact (median / max, s) | 4.2 / 25.9 |
| Clef plus 27B for what stays undecided (median / max, s) | 4.2 / 25.9 |
| files with review lines found by code (covered without the model) | 75% (47/63) |
| decided rate, Q1 "does the review cover this file" | 97% (61/63) |
| decided rate, Q2 "defect the review does not name" | 83% (52/63) |
| reviews with every file covered (fold true / undecided / false) | 10 / 0 / 4 of 14 |
| reviews where Q2 finds an unnamed defect in some file (best guess) | 0% (0/14) |
| reviews where the 27B recorded an unmentioned defect (no_silent_approval = 0) | 0% (0/14) |
| agreement of Q2 with the recorded no_silent_approval | 100% (14/14) |

Notes. Three quarters of files have a review line that cites them, found by code with no model call; for those the coverage question is settled by set arithmetic. Clef decided both questions on most files (97% and 83%), so decidability is not the obstacle. Q2 found no unnamed defect in any of the 14 approve reviews, and the 27B recorded none either, so the comparison cannot say whether Q2 would find a real one; the set has no review with a seeded miss. Latency grows with the diff (11.6 s max for a 4,800-token file).

Call: **drop Q2 (the owner already rules open-ended defect questions out); keep Q1 only as a code check** (the share of changed files no review line cites), which needs no model.

### 7. Contracts (added scope): `meets_intent`, `follows_guide`, `adds_unasked`

Declaration (REST; the JSON Schema variant swaps `openapiOperations` for `schemaProperties`, the only difference):

```ts
scopes.guarantees = { split: jsonItems(guarantees), piece: 'guarantee', context: { mechanisms: related(openapiOperations, overlap, top 4), choices: related(jsonItems choices, overlap, top 3) } };
scopes.choices    = { split: jsonItems(choices, max 8), piece: 'choice', context: { guide: related(markdownSections of the guide, overlap, top 3) } };
scopes.parts      = { split: openapiOperations(document, max 8), piece: 'part', context: { intent: whole(intent) } };
checks.meets_intent  = all over guarantees;  checks.follows_guide = all over choices;  checks.adds_unasked = all over parts ("asked for", flagWhen false)
```

**meets_intent**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 4 / 6 |
| tokens per chunk (median / max, cap 8192) | 1454 / 2054 |
| oversize chunks (escalate unasked) | 0 of 18 |
| Clef latency per chunk (median / max, s) | 1.3 / 1.9 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 89% (16/18) |
| artifacts whose check is decided by the fold | 75% (3/4) |
| Clef time per artifact (median / max, s) | 5.5 / 9.9 |
| Clef plus 27B for what stays undecided (median / max, s) | 6.0 / 55.5 |
| today, whole artifact (median / max, s) | 25.4; the 27B answered all 4 recorded verdicts |
| agreement with the recorded 27B answer (all four specs passed) | 100% (4/4); decided by the fold: 75% (3/4) |

**follows_guide**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 8 / 8 |
| tokens per chunk (median / max, cap 8192) | 1225 / 1475 |
| oversize chunks (escalate unasked) | 0 of 32 |
| Clef latency per chunk (median / max, s) | 1.1 / 4.3 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 88% (28/32) |
| artifacts whose check is decided by the fold | 75% (3/4) |
| Clef time per artifact (median / max, s) | 9.6 / 12.6 |
| Clef plus 27B for what stays undecided (median / max, s) | 10.4 / 109.3 |
| today, whole artifact (median / max, s) | 25.4; the 27B answered all 4 recorded verdicts |
| agreement with the recorded 27B answer (all four specs passed) | 50% (2/4); decided by the fold: 75% (3/4) |

**adds_unasked**

| measure | value |
| --- | --- |
| chunks per artifact (median / max) | 5 / 6 |
| tokens per chunk (median / max, cap 8192) | 678 / 896 |
| oversize chunks (escalate unasked) | 0 of 21 |
| Clef latency per chunk (median / max, s) | 0.7 / 0.9 |
| decided rate, chunk answers >= 0.8 or <= 0.2 | 90% (19/21) |
| artifacts whose check is decided by the fold | 75% (3/4) |
| Clef time per artifact (median / max, s) | 2.8 / 4.3 |
| Clef plus 27B for what stays undecided (median / max, s) | 3.3 / 52.6 |
| today, whole artifact (median / max, s) | 25.4; the 27B answered all 4 recorded verdicts |
| agreement with the recorded 27B answer (all four specs passed) | 75% (3/4); decided by the fold: 75% (3/4) |

Notes. Each of the three folds decides on 3 of 4 specs, with about 90% of chunk answers decided and 0.7 to 1.3 s per chunk (a different spec is undecided for each check). Clef time is 3 to 10 s per spec and check against about 25 s for one escalation, but an undecided fold still costs its unknown chunks in 27B calls. `follows_guide` disagrees with the 27B on two specs (the fan-out says some choice departs from the guide); I did not adjudicate those against the guide, so I cannot say which side is right. The chunking caps (8 choices, 8 operations) mean a large spec is only partly judged; that is a choice for budget and it hides items, so it should come out before use.

Call: **keep as fan-out** for `meets_intent` and `adds_unasked`; `follows_guide` needs the disagreements adjudicated first.

## Splitters: deterministic, model, and fallback

The owner's rule: deterministic input gets a deterministic splitter, free-form input gets a model splitter, and a deterministic splitter that finds its input off-shape falls back to the model. The declaration carries it as `split`:

```ts
split: { name: 'sentences', ..., expect: { min: 1, max: 5, maxChars: 200 }, fallback: { name: 'llm', from: 'request', recorded: 'llm:asks' } }   // deterministic with fallback
split: { name: 'llm', from: 'request', recorded: 'llm:asks' }                                                                                     // model only
split: { name: 'llm', ..., sweep: { name: 'sentences', ... } }                                                                                    // model plus a sweep of the leftover text
```

**The model splitter was simulated.** The spike may only use Clef, so `llm-splits.ts` holds hand-written splits (mine) that stand in for the 27B's structured output, in the shape the owner described: each item carries the exact source span, and the `llm` chunker drops any item whose span is not in the input (`rejected` counts them; it dropped none here, and a unit test covers a made-up span). The gold items I scored against are the same strings, so the model rows are a ceiling for what a careful model finds, not a measurement of the 27B. What the comparison does show is what the deterministic splitters miss and what each variant costs in Clef calls. Replays would reuse the memoized split; that was not built.

The `expect` limits (5 items, 200 characters per item, 6 promises) were chosen before the splitter runs except `maxChars`, which was 300 first and did not fire on the one request it should have; I changed it to 200 after seeing that, so treat it as tuned.

**Splitters on research request asks, the 20 labelled cases**

| splitter | artifacts | items (median / max) | gold found | gold isolated in its own item | items with no gold | Clef calls per artifact (median / max) | fallback ran |
| --- | --- | --- | --- | --- | --- | --- | --- |
| deterministic (sentences + clause split) | 20 | 2 / 3 | 100% (42/42) | 100% (42/42) | 0% (0/42) | 2 / 3 | no |
| deterministic with model fallback | 20 | 2 / 3 | 100% (42/42) | 100% (42/42) | 0% (0/42) | 2 / 3 | no |
| model (simulated) | 20 | 2 / 3 | 100% (42/42) | 100% (42/42) | 0% (0/42) | 2 / 3 | no |
| model + sweep of leftover text | 20 | 2 / 3 | 100% (42/42) | 100% (42/42) | 0% (0/42) | 2 / 3 | no |

**Splitters on research request asks, two real prod requests (free-form, with a context paragraph)**

| splitter | artifacts | items (median / max) | gold found | gold isolated in its own item | items with no gold | Clef calls per artifact (median / max) | fallback ran |
| --- | --- | --- | --- | --- | --- | --- | --- |
| deterministic (sentences + clause split) | 2 | 5 / 6 | 85% (22/26) | 19% (5/26) | 0% (0/11) | 5 / 6 | no |
| deterministic with model fallback | 2 | 10 / 16 | 92% (24/26) | 92% (24/26) | 8% (2/26) | 10 / 16 | prod-18133686 (more than 5 items); prod-a16af0cc (an item over 200 characters) |
| model (simulated) | 2 | 10 / 16 | 92% (24/26) | 92% (24/26) | 8% (2/26) | 10 / 16 | no |
| model + sweep of leftover text | 2 | 11 / 18 | 92% (24/26) | 92% (24/26) | 17% (5/29) | 11 / 18 | no |

**Splitters on PR description promises, 5 reviews**

| splitter | artifacts | items (median / max) | gold found | gold isolated in its own item | items with no gold | Clef calls per artifact (median / max) | fallback ran |
| --- | --- | --- | --- | --- | --- | --- | --- |
| deterministic (sentences + delivery-verb filter, cap 5) | 5 | 5 / 5 | 59% (10/17) | 47% (8/17) | 59% (13/22) | 5 / 5 | no |
| deterministic with model fallback (no cap; > 6 items falls back) | 5 | 4 / 5 | 100% (17/17) | 100% (17/17) | 15% (3/20) | 4 / 5 | #2170 (more than 6 items); #2161 (more than 6 items); #2149 (more than 6 items) |
| model (simulated) | 5 | 4 / 5 | 100% (17/17) | 100% (17/17) | 0% (0/17) | 4 / 5 | no |
| model + sweep of leftover text | 2 | 11 / 13 | 100% (9/9) | 100% (9/9) | 63% (15/24) | 11 / 13 | no |

**Check outcome by splitter (best guess; ? = undecided)**

| artifact | check | splitter | items | folded value | swept items | rejected spans |
| --- | --- | --- | --- | --- | --- | --- |
| prod-18133686 | answers_every_ask/asksDet | sentences | 6 | ? (guess true) | 0 | 0 |
| prod-18133686 | answers_every_ask/asksAuto | sentences -> llm | 10 | ? (guess false) | 0 | 0 |
| prod-18133686 | answers_every_ask/asksLlm | llm | 10 | ? (guess false) | 0 | 0 |
| prod-a16af0cc | answers_every_ask/asksDet | sentences | 5 | ? (guess true) | 0 | 0 |
| prod-a16af0cc | answers_every_ask/asksAuto | sentences -> llm | 16 | false (guess false) | 0 | 0 |
| prod-a16af0cc | answers_every_ask/asksLlm | llm | 16 | ? (guess false) | 0 | 0 |
| #2170 | promises_handled/promisesDet | sentences | 5 | true (guess true) | 0 | 0 |
| #2170 | promises_handled/promisesAuto | sentences -> llm | 4 | true (guess true) | 0 | 0 |
| #2170 | promises_handled/promisesLlm | llm | 4 | true (guess true) | 0 | 0 |
| #2161 | promises_handled/promisesDet | sentences | 5 | true (guess true) | 0 | 0 |
| #2161 | promises_handled/promisesAuto | sentences -> llm | 5 | true (guess true) | 0 | 0 |
| #2161 | promises_handled/promisesLlm | llm | 5 | true (guess true) | 0 | 0 |
| #2149 | promises_handled/promisesDet | sentences | 5 | true (guess true) | 0 | 0 |
| #2149 | promises_handled/promisesAuto | sentences -> llm | 4 | true (guess true) | 0 | 0 |
| #2149 | promises_handled/promisesLlm | llm | 4 | true (guess true) | 0 | 0 |
| #2183 | promises_handled/promisesDet | sentences | 2 | true (guess true) | 0 | 0 |
| #2183 | promises_handled/promisesAuto | sentences | 2 | true (guess true) | 0 | 0 |
| #2183 | promises_handled/promisesLlm | llm | 1 | true (guess true) | 0 | 0 |
| #2179 | promises_handled/promisesDet | sentences | 5 | true (guess true) | 0 | 0 |
| #2179 | promises_handled/promisesAuto | sentences | 5 | true (guess true) | 0 | 0 |
| #2179 | promises_handled/promisesLlm | llm | 3 | true (guess true) | 0 | 0 |
| prod-18133686 | answers_every_ask/asksHybrid | llm | 11 | ? (guess false) | 1 | 0 |
| prod-a16af0cc | answers_every_ask/asksHybrid | llm | 18 | ? (guess false) | 2 | 0 |
| #2170 | promises_handled/promisesHybrid | llm | 13 | true (guess true) | 9 | 0 |
| #2161 | promises_handled/promisesHybrid | llm | 11 | true (guess true) | 6 | 0 |

What it shows.
- **Shaped input is fine deterministic.** The 20 case requests are one or two clauses; the clause splitter found every labelled ask, and the fallback never fired. The model splitter adds nothing there.
- **Free-form input needs the model splitter.** On two real prod requests (a context paragraph, a question with a long list, and a six-item "for each indicator give..." paragraph), the deterministic splitter found 85% of the labelled asks but isolated only 19% of them; a clause such as "Cover tests that assert their own mocks, recompute the expected value... or test the framework" stays one 230-character item. The fallback fired on both (more than 5 items; an item over 200 characters). The model split found 24 of 26 and isolated 24 of 26; the two it missed are my own rewrite of "and sources" which the containment check does not match, an artifact of the scoring, not of the split.
- **PR descriptions are free-form.** The delivery-verb sentence splitter with a cap of 5 found 10 of the 17 labelled promises and chose bug-narrative sentences over the fix on #2170, #2161 and #2149. With a fallback on "more than 6 candidate sentences" it fell back on exactly those three, and reached 17 of 17. On #2183 and #2179 (short, clean descriptions) it did not fall back and kept 2 and 5 sentences, three of which carry no labelled promise.
- **Clef calls per artifact** are the number of items: the model split made 1 to 5 promise chunks per review, no more than the capped deterministic one, and 10 to 16 ask chunks on the free-form requests (against 5 to 6 for the deterministic splitter that missed items), so the extra calls buy recall. Each model split is itself one 27B call per artifact version; that cost is not in the tables.
- **The sweep is not worth its cost on these inputs.** Sweeping the text no span covers added 1 to 2 ask chunks on the real requests and 9 and 6 promise chunks on two reviews, none of the 15 promise chunks carrying a labelled promise, for no gain in recall (the model split was already at 100% on the promises and 92% on asks). It would matter if the model splitter dropped items, so keep it as an option for artifacts where a missed item is costly, and expect to pay 2 to 3 times the Clef calls.
- **Verdicts barely changed.** The folded outcome was the same under every splitter on the five reviews; on the two real requests it was undecided under most variants and false (an ask unanswered) under one, the fallback on the tautological-test request. The splitter changes what is judged, and on these artifacts nothing was wrong enough for it to change the answer. That is a limit of the data, not a point for the deterministic splitter.

## Recommendation: judge evidence API

Today a scope is a list of units the extension builds in code, and a check names a scope and asks its question once per unit; the verdict's per-check score is the share of units that held. Replace the hard-coded `Evidence` builder with declarations over strategies registered with the judge.

```ts
type Strategy<O> = { name: string; run: (inputs: Inputs, options: O) => Chunk[] };         // a splitter; extensions may register their own
type Aggregator = { name: string; fold: (answers: ChunkAnswer[], options: AggOptions) => Tri };  // all | any | share | atLeast | coverageUnion
type Digest = { extract: (text: string) => Fact[]; compare: (a: Fact[], b: Fact[]) => number }; // optional, for map-reduce

type Scope = {
  split: { name: string } & Options & { expect?: Shape; fallback?: { name: string } & Options; sweep?: { name: string } & Options };
  context?: Record<string, { name: string } & Options>;   // selectors adding named state to each chunk
  piece?: string;
};
type CheckDecl = { scope: string; question: Question; aggregate: { name: string } & Options; flagWhen: boolean };
```

The extension provides `inputs` (named texts: report, request, diff, review, guarantees...), names the splitters for its kinds, and may register splitters the library lacks. The judge owns the library of strategies in this spike, the three-valued fold (so a check is decided as soon as the unknown chunks cannot change it, and only then escalates), per-version memoization of model splits, span verification, the `expect` shape check with fallback, and the verdict field recording which splitter ran and why a fallback fired.

One change to the verdict follows. A fold of `all` over N chunks replaces the share-of-units average for these checks, so a check's `met` becomes a decided boolean with the failing chunks as its notes; `share` and `atLeast` keep the fractional form where a check wants one. The library is generic enough that research and review share strategies: `sentences`, `listItems`, `related` (overlap) and the `all` fold run on research asks, summary sentences, PR promises and contract guarantees without a line of kind-specific code. Two checks on one artifact need different strategies in every kind measured: a research report uses `sentences`/`all` for its asks and `digestPairs`/`any` for contradictions; a review uses `sentences`/`all` over description promises and `diffFiles`/`coverageUnion` over changed files.

## What the spike does not show

- **No defect-catching claim for reviews or contracts.** Every recorded review approved and every recorded contract spec passed, and the 27B's recorded answers say "nothing missing" for `promises_handled` on all 14. Agreement there means no false block, not a catch. The only labelled catch/false-block data is the 20 research cases (4 seeded-bad for asks, 2 for summary and for contradiction, 5 good).
- **The 27B is not measured.** Its recorded verdicts are a reference, not truth, and the model splitter is simulated by hand-written splits.
- **Small and hand-built.** The research set is short invented pages; its contradictions are single numbers, which the extractor suits. The 14 reviews are the newest approves, three quarters under 25 KB of diff. Four contracts, one JSON Schema.
- **Latency is Clef only,** from inside the container, one request at a time, while prod ran research sessions on the same host. Today's path timings assume 25 s per escalation, not a measured run.
- **Not built:** Clef picking hunks from a menu of headers, per-version memoization, `dtsExports` and `relevanceWindow` on any input (they are in the library and not exercised), and the effect of fan-out on a cost budget with several checks sharing a scope.
- **Token cap:** chunk estimates use the judge's 4-bytes-per-token rule, not Clef's tokenizer.

## Rerunning

`fetch.ts`, `fetch-diffs.ts` and `fetch-contracts.ts` pull the data read-only into `tools/spike-fanout/data/` (not committed: it holds prod research conversations); `run.ts research|baseline|review|contracts|splitters|prod-stats` asks Clef and writes `out/*.json` (also not committed, with a request cache that makes reruns free); `tables.ts` renders the tables above. `DRY=1` chunks without calling Clef.
