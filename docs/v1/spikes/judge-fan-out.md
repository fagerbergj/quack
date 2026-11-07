# Judge fan-out spike

Question: does judging chunks of an artifact (fan-out) and combining the chunk answers by declared rules (fan-in) make grading faster and more decidable than today's whole-artifact checks, on real research reports and PR reviews?

Short answer: it is much more decidable and faster for the checks whose input splits into items (promises, contract guarantees and parts, summary sentences, fact pairs), and it is not a free win for `answers_every_ask`, where the per-ask question was worse than the whole question on the labelled set. Deterministic splitters are enough for structured input (diff, OpenAPI, JSON Schema, Markdown sections, bullets) and fail on free-form prose (requests, PR descriptions), where a model splitter with span verification found every labelled item.

Round 2 (below) reran the candidates on 76 recorded versions, 62 of them rejected, added Sleeper and every prod research report, and timed both paths from the sessions' durable logs. It changes two calls: `sibling_parts` is dropped outright, and `promises_handled` is kept only with linked-issue text and the model splitter. Fan-out is about 14 times faster than today's review pass and about 7 times for contracts, but today's pass also runs checks the fan-out does not, so read those ratios as an upper bound.

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

Call: **drop** (round 2 strengthened this; see "Changes to the keep or drop calls"). The chunking works and is nearly free in code; the pair question ("lacks a part") is an open-ended difference and Clef did not decide it. Do not weight it until there are 20 or more triggers.

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

## Round 2: rejected artifacts, Sleeper, research and timing

Round 1 measured the candidates on mostly approved artifacts. Round 2 reruns them on the versions the judge rejected, adds Sleeper and every prod research report, and times both paths. It used 498 more Clef requests by my count (the cap was 500), one at a time; the 27B was never called and no session was started.

### Data

Every judged version in the sessions of interest was read from prod with `artifacts <key>` (read-only): 722 `github.review` versions (200 rejected), 49 `contract.spec` (25 rejected), 43 `sleeper.cards` (21 rejected), 17 `research.report` (11 rejected). The budget covered 76 of them, in the owner's order: first rejected versions where a covered check was flagged, then Sleeper and research passes, then reviews.

| kind | versions run | of which rejected | left out |
| --- | --- | --- | --- |
| `github.review` | 27 (+10 for the sibling control) | 27 | 173 other rejected versions, all passing ones |
| `contract.spec` | 6 | 6 | 19 other rejected (gate-only failures, or the same spec revised), 24 passing |
| `sleeper.cards` | 19 | 16 | 5 rejected; `sleeper.trade` (its cards hold no suggestions, so no row exists to judge) |
| `research.report` | 14 | 8 | 3 rejected (earlier versions of reports already run) |

Agreement is only computed where the recorded verdict used the rubric version the declarations were written from (reviewer@4, contract@1, research@3 and the current Sleeper rubrics); 40 of the 76 versions were recorded under an older rubric, where the question text differs, so they appear in the adjudications and the timing but not in the counts. `sleeper.waivers@1` to `@3` even made `bye_coverage` a positive check.

### Agreement with the recorded verdicts

"Flagged" uses the check's own polarity (a positive check is flagged below 1, a pitfall above 0). "Only recorded" is a flag the fan-out did not raise; "undecided" counts versions where the fold could not decide and would escalate.

| kind | check | recorded verdicts | n | both flag | only recorded | only fan-out | neither | fan-out undecided |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| contract.spec | adds_unasked | rejected | 6 | 0 | 3 | 2 | 1 | 3 |
| contract.spec | follows_guide | rejected | 6 | 0 | 2 | 0 | 4 | 2 |
| contract.spec | meets_intent | rejected | 6 | 1 | 3 | 1 | 1 | 4 |
| github.review | promises_handled | rejected | 13 | 0 | 2 | 0 | 11 | 3 |
| github.review | sibling_parts | rejected | 12 | 10 | 2 | 0 | 0 | 3 |
| research.report | answers_every_ask | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| research.report | answers_every_ask | rejected | 2 | 0 | 0 | 0 | 2 | 1 |
| research.report | no_self_contradiction | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| research.report | no_self_contradiction | rejected | 5 | 0 | 2 | 0 | 3 | 1 |
| research.report | summary_follows_findings | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| research.report | summary_follows_findings | rejected | 5 | 0 | 2 | 0 | 3 | 3 |
| sleeper.cards digest | game_flow_from_score | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards digest | luck_without_margin | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards digest | story_in_summary | passed | 1 | 0 | 0 | 0 | 1 | 1 |
| sleeper.cards history | aggregated_not_scolded | passed | 1 | 0 | 1 | 0 | 0 | 1 |
| sleeper.cards history | aggregated_not_scolded | rejected | 3 | 3 | 0 | 0 | 0 | 3 |
| sleeper.cards history | decision_time_grading | passed | 1 | 0 | 0 | 0 | 1 | 1 |
| sleeper.cards history | decision_time_grading | rejected | 3 | 0 | 0 | 0 | 3 | 3 |
| sleeper.cards history | efficiency_as_record | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards history | efficiency_as_record | rejected | 3 | 0 | 0 | 0 | 3 | 0 |
| sleeper.cards history | luck_process_split | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards history | luck_process_split | rejected | 3 | 0 | 0 | 0 | 3 | 0 |
| sleeper.cards history | single_week_as_pattern | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards history | single_week_as_pattern | rejected | 3 | 0 | 0 | 0 | 3 | 0 |
| sleeper.cards lineup | close_call_news | rejected | 1 | 1 | 0 | 0 | 0 | 0 |
| sleeper.cards lineup | designation_weighted | rejected | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards lineup | matchup_overrides_talent | rejected | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards lineup | picks_an_action | rejected | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards lineup | weather_mentioned_unmoved | rejected | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards retro | hindsight_called_process | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards retro | lessons_actionable | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards retro | summary_names_swing | passed | 1 | 0 | 0 | 0 | 1 | 0 |
| sleeper.cards trends | aggregator_only | rejected | 2 | 1 | 0 | 1 | 0 | 0 |
| sleeper.cards trends | conflict_stated | rejected | 2 | 2 | 0 | 0 | 0 | 0 |
| sleeper.cards trends | original_source_named | rejected | 2 | 2 | 0 | 0 | 0 | 0 |
| sleeper.cards waivers | bye_coverage | rejected | 2 | 0 | 2 | 0 | 0 | 2 |
| sleeper.cards waivers | close_call_news | rejected | 2 | 0 | 2 | 0 | 0 | 2 |
| sleeper.cards waivers | crowd_metric_as_value | rejected | 2 | 0 | 2 | 0 | 0 | 0 |
| sleeper.cards waivers | ordered_fallback_flagged | rejected | 2 | 0 | 0 | 1 | 1 | 1 |
| sleeper.cards waivers | ranked_on_usage | rejected | 2 | 2 | 0 | 0 | 0 | 2 |
| sleeper.cards waivers | sized_to_waiver_type | rejected | 2 | 0 | 0 | 0 | 2 | 1 |

What the rows say, with the adjudications below behind them.

- The fan-out raised the same flag as the recorded verdict on the structured Sleeper rows (history, trends, lineup close calls, waivers `ranked_on_usage`) and on 10 of 12 triggered `sibling_parts` pairs (see below for why that second figure means little).
- Where only the recorded verdict flagged (23 check-versions in the table), the adjudications below split three ways: the fan-out was blind to something I can name (an input it was not given, a splitter that skipped the sentence, my 8-item caps), undecided (so the 27B would have decided), or the recorded flag was the wrong one.
- The review rows are biased by selection: I chose rejected versions on purpose, so "neither" is rare.

### Adjudications

I read the artifact behind every disagreement on current-rubric and older versions alike. Neither side is ground truth; "fan-out right" means my reading of the artifact agrees with it.

**Contracts (6 rejected specs)**
- `608df011` v11 and v12, `meets_intent`: recorded flagged because the document's `$ref` to `#/$defs/service` does not resolve, so "it cannot enforce any guarantee". Every guarantee has a mechanism in the text (fan-out p 0.79 to 0.97), and the broken `$ref` is already a failed code gate (`document_clean`). **Fan-out right on the question asked**; the 27B counted the same defect twice.
- `d87192eb` v2, `meets_intent`: recorded flagged that PATCH is missing on both paths although a choice says all seven methods are declared. The document does lack PATCH. **Recorded right.** The fan-out cannot see it: the check is per guarantee, and the contradiction is between a choice and the document. A per-choice question with the choice's `pointer` resolving to its document part would catch it; that needs a JSON-pointer selector the library lacks.
- `608df011` v11 and `0e13c5c3` v2, `follows_guide`: recorded names a choice that departs from the guide without a reason (`exec-policy-open`; `resume-409`). Neither is among the first 8 choices my declaration read (the specs have 24 and 18). **Fan-out blind by my cap, not wrong.** The cap hid items in 3 of 6 rejected specs and should come out.
- `5be69037` v4, `adds_unasked`: recorded flagged (no note) the five-method surface. v9, also five methods, passed. **Recorded inconsistent**; the fan-out (all five asked for, p 0.89 to 0.97) is consistent; I did not establish what the owner wanted.
- `d87192eb` v2 and `0e13c5c3` v2, `adds_unasked`: recorded flagged with no note; the fan-out saw only 8 operations of up to 14. **Undetermined.**
- `608df011` v11 and v12 `adds_unasked`, only fan-out flagged: Clef was unsure (0.49 to 0.53 for `exec` and `timeZone`); the recorded note maps every field to the intent. **Recorded right; the fold was undecided, so it would have escalated, not blocked.**

**Reviews (27 rejected versions, `promises_handled` recorded-flagged on 7 of them)**
- #1919 v1 (a linked issue's "Done when" asks for Playwright screenshots) and #1881 v2 (a linked issue asks for jitter): **recorded right.** My PR data held no linked-issue bodies, so the fan-out never saw the promises. An input gap, not a question gap: the issue bodies must be supplied.
- #1894 v5 (description says starting a session sends the typed text; the diff only seeds the composer): **recorded right.** The deterministic splitter did not select that sentence. The model splitter's job.
- #1879 v5 (description claims work already merged elsewhere): recorded plausibly right; the fan-out was undecided on all five chunks (missing 0.40 to 0.48) and would have escalated.
- #2006 v4 and #1864 v4: the recorded flag came from a check whose resolver was `none` (no resolver decided), counted as unmet. **Fan-out right** (the chunks are delivered or not promises, decided).
- #2061 v4 (an LSP bullet already satisfied by the base commit): inconclusive; the review itself calls it satisfied vacuously.
- #1881 v4, only the fan-out flagged: recorded passed it (Clef resolved it). The deterministic splitter had picked bug-narrative sentences ("Nothing starts a run for them, so the caller hangs") that are not promises, and Clef was undecided on them. **Recorded right.**
- So `promises_handled` raised none of the 4 recorded flags that were right (one of them undecided), and none of the misses was the question's fault: two were missing linked-issue text, one a sentence the splitter never offered, one undecided chunks.

**Reviews, `sibling_parts` (rejected)**
- Of the 99 rejected versions flagged on `promises_handled` or `sibling_parts`, 87 are flagged on `sibling_parts`, and only 22 of those add a new file with a same-suffix sibling. 78 of the 87 have `sibling_parts` as the only flagged one of the two, and 20 of those 78 have the trigger; for the other 58 the fan-out has no unit.
- Where a trigger exists, Clef says "the sibling has a part the new file lacks" at p 0.77 to 0.92 on 4 of the 5 control passes too (reviews whose recorded verdict was not flagged), and adding the review's lines to the chunk changed nothing (#1884 v1 and v2: 0.907 and 0.907). Two of the ten pairs exceeded Clef's real input limit. The question is nearly always yes, so it carries no information.
- The recorded flags in the untriggered versions: #2185 v1's note says the new kind "carries all the same fields... no missing structural parts" and flags it anyway (**recorded wrong**); #1940, #1943 and #1944 are flagged because the review "never enumerates" what the siblings carry while saying the new code is a faithful port (**false blocks of the kind the owner wants gone**); #2006 v4, #1864 v4 and #1894 v5 have no new instance at all.

**Research (the ten from 2026-11-05 plus earlier; 11 rejected)**
- Four of the 11 rejected versions (`ui:4962efd3` v1 and v2, `ui:92b69070` v1, `ui:9d5db599` v1) had a model check flagged; the other 7 failed the code gates (`quotes_exist`, `findings_cited`), which the fan-out does not touch. `ui:92b69070` and `ui:9d5db599` were flagged on `supported_by_source` and `contradicted_by_source`, which cannot run here (below).
- `ui:4962efd3` v1 and v2, `summary_follows_findings`: v1's Summary says every vendor publishes self-reported scores while a finding says LangMem publishes none; v2 says reproductions are "10 to 60 points" lower while the largest gap in the findings is about 36. **Recorded right on both.** The fan-out answered 0.55 and 0.59: undecided, so the 27B decides, which is the right outcome for a subtle overreach.
- `answers_every_ask` on those two: the request has no ask the deterministic splitter finds (0 chunks), so the fan-out had nothing to judge. Six of the 17 prod requests behave this way; the declared fallback (`fewer than 1 items`) would fire for each. The model splits for them are not simulated, so this candidate is unmeasured on real requests.
- `no_self_contradiction` on `4962efd3` v1 and v2: recorded flagged with no note; the three pairs the extractor surfaced were decided "no conflict" (p 0.02 to 0.11). I could not find the contradiction the 27B saw in a 13 KB report with dozens of figures; the pair cap (3) may hide it. **Undetermined.**
- `supported_by_source` and `contradicted_by_source` (today's kept checks) through the library, on the 20 labelled cases: one chunk per finding with the cited source's best-matching sentences as context. `supported_by_source` catches 3 of 4 seeded-bad and false-blocks 0 of 5 good; `contradicted_by_source` catches 3 of 3 and false-blocks 0 of 5; the fold decides 18 and 17 of 20 cases. On the prod versions these two cannot run: the source texts live in each session's fetch log, not in the artifact, so I did not run them there.

**Sleeper (19 versions of 6 kinds; the library covers the per-row and per-card model checks)**
- `trends`: `conflict_stated` and `original_source_named` agree on both rejected versions; `aggregator_only` agrees on one and differs on the other: the fan-out flagged items whose only source is Sleeper's own add counts (p 0.78 to 0.80) that the 27B accepted. **Undetermined.**
- `history` (`aggregated_not_scolded`): all three rejected versions flagged by both; the fan-out was undecided on every one (the season rows are large), so it flags but would escalate. On the passing v2 the 27B flagged single-season criticisms as not a pattern (its note says so); the fan-out answered yes at 0.80 to 0.93. **Undetermined.**
- `waivers` (current rubric, two rejected versions): `crowd_metric_as_value` recorded-flagged, fan-out not (p 0.05 to 0.19). The flagged claim says the Bears DEF is "at just 21.1% owned, safe at priority 8": ownership as availability, which the rubric's own `ranked_on_usage` allows as scarcity. **Fan-out right**, and the same on waivers v1 and v3 from the older rubrics ("16.3% owned, Taylor is the safest TE still available"). `close_call_news`: the card attaches a Rams press-conference item to Blake Corum and admits it does not name him; the 27B flagged it, Clef said 0.68 to 0.93 yes. **Recorded right, fan-out undecided.** `bye_coverage`: recorded flagged one claim without a note; Clef leaned no (0.13 to 0.32). **Undetermined.**
- `lineup` v2 (@4): `close_call_news` both flag. On the older v2: `matchup_overrides_talent` recorded-flagged one of two swapped rows (Williams 9.77 starts over Concepcion 10.22 at FLEX, the reverse at WR2); Clef said 0.05 and 0.04 to both. **Undetermined**: the FLEX swap does bench the higher projection, and the card's reason decides whether it was matchup alone.
- `retro`: v1 `summary_names_swing`, recorded: the summary names 17.7 as the biggest swing when the data's largest is 23; the fan-out saw only the summary text (0.97) and cannot check the number. **Recorded right.** It needs the card's `swings` as context, which the declaration did not include.
- Sleeper checks that are **not pure declarations in this library**: the units of `differs`, `close`, `known`, `consolidation`, `suggested`, `conflict`, `sourced` and `preview` are computed from the job's facts (the baseline lineup, `closeCalls(f)`, designations against news), not from the card; the facts are not in the artifact (they are in the workflow task's result on prod). Two library strategies would close that: a `join` selector (a row's facts looked up by a key into a second input) and a `where` that can compare against a joined value. `where` on fields the card carries, `pick` and a `json` selector were added and cover `picks_an_action`, `designation_weighted`, `close_call_news` (approximated by "has news"), `matchup_overrides_talent` (approximated by "action is not keep"), the history, trends, digest and waivers row checks and every card-level summary check. `names_baseline_it_beats`, the trade checks and the lineup `differs` scope were not expressible and were not run.

### Timing

Today's path, measured from each session's durable log (`session.sqlite` in `quack-v1`, read-only): the tool task of the submit call (`submit_review`, `submit_contract`, `submit_report`) is the judge pass, matched to its judged version by order (714 review passes, 49 contract, 5 research). Fan-out: the summed Clef latency of the requests (concurrency 1, which is the honest figure) plus the splitter and fold time measured in code; every fan-out pass here is the candidates only, while today's pass runs every model check of the rubric, so the ratio overstates the gain. For Sleeper the judge is a step inside one workflow task per run (`quack.workflow.sleeper.*`), so the table shows the whole run, which includes the agent's own generation; the judge's share cannot be separated from the log.

| kind | versions run | Clef calls per artifact (median / max) | fan-out pass, concurrency 1 (median / max, s) | of which code (splitter + fold, ms) | today: judge-pass tasks (n) | today: all passes (median / max, s) | today: passes over 3 s (median / max, s) | ratio of medians (today over 3 s / fan-out) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| github.review | 37 | 5 / 8 | 15.0 / 47.5 | 3 / 21 | 1381 | 77.3 / 1153.3 | 207.5 / 1153.3 | 14x |
| research.report | 14 | 6 / 11 | 3.6 / 17.3 | 4 / 29 | 18 | 0.4 / 1682.0 | 527.3 / 1682.0 | 148x |
| contract.spec | 6 | 18 / 22 | 17.3 / 23.5 | 3 / 4 | 65 | 96.9 / 491.0 | 124.0 / 491.0 | 7x |
| sleeper.cards | 19 | 6 / 17 | 3.9 / 14.9 | 0 / 1 | 38 | 185.2 / 2324.3 | 189.2 / 2324.3 | 49x |


Today's pass, from the log (paired passes):

| kind | passes | median pass | recorded 27B escalations per pass (median) | seconds per escalation (median) | queue-inflated (over 60 s per escalation) |
| --- | --- | --- | --- | --- | --- |
| `github.review` | 714 | 207 s | 9 | 24 | 28 of 712 (4%) |
| `contract.spec` | 49 | 124 s | 3 | 41 | 13 of 49 (27%) |
| `research.report` | 5 | 527 s | 19 | 25 | 0 of 5 |

- The assumed 25 s per serial escalation is what reviews and research actually take (24 s and 25 s). Contracts take 41 s per escalation, and 13 of 49 contract passes are longer than 60 s per escalation, which I read as slot queueing or a long document; I cannot tell from the log which.
- Slot waits are not recorded as such. The inflation test is passes whose time per escalation exceeds 60 s, plus the observation (from the live session list while I was fetching) that several researcher sessions sat at "waiting for a model slot".
- Fan-out per artifact: reviews 5 requests and 15 s median (47 s max, a promises chunk of 2,500 tokens takes about 2.3 s on Clef), contracts 18 requests and 17 s, research reports 6 requests and 3.6 s, Sleeper cards 6 requests and 3.9 s. The code (splitters, retrieval, folds) costs 0 to 30 ms. Clef latency scales with input: 0.2 s for a 150-token pair, 1.2 s for 1,400 tokens, 2.3 s for 2,200.
- **Concurrency.** I sent the 17 requests of one contract spec (`5be69037` v4) uncached, one at a time and four at a time: 19.7 s wall one at a time (18.3 s summed Clef latency, so the ssh pipe adds about 7%), 18.9 s with four in flight (median latency per request 4.7 s against 1.2 s). Clef serves one request at a time, so parallelism buys nothing; the numbers above are the serial ones.
- A real limit turned up. Clef rejected 4 requests (9,628 to 10,585 tokens by its tokenizer) that the judge's 4-bytes-per-token rule had estimated under 8,192; all were a new test file plus its sibling. The 4-byte rule is too optimistic on code, so the judge would send such a request and get an error instead of escalating on the cap.

### Changes to the keep or drop calls

- **`sibling_parts`: drop as a Clef fan-out, and do not keep it as a 27B check either** without a structural trigger. 58 of 78 sibling-only failures have no new same-suffix file at all, Clef answers yes to the pair regardless of the review, and the recorded flags I read include a note that contradicts its own flag and "never enumerates" false blocks.
- **`promises_handled`: keep, with two conditions.** Supply linked issue bodies as a promise source, and use the model splitter on free-form descriptions. Without both it raised none of the flags that were right; the 27B's own record includes two flags from a `none` resolver that the fan-out correctly did not raise.
- **`answers_every_ask`: still needs a different question, and the splitter fallback is not optional**: 6 of 17 prod requests have no ask for the deterministic splitter.
- **`summary_follows_findings`: keep, as a decider that escalates**: it never decided a subtle overreach the wrong way (0.55 and 0.59), and it decides the blatant ones the whole-report question misses.
- **`no_self_contradiction`: keep, undetermined on real reports** (the pair cap is a guess; two recorded flags could not be reproduced).
- **Contracts: keep `meets_intent` and `adds_unasked`, take the 8-item caps out, add a per-choice "does the document implement this choice" check using the choice's pointer, and let `follows_guide` run on all choices.** The fan-out was right on 2 of the 4 disagreements I could settle and blind on 3 because of the caps.
- **Sleeper: keep as per-row fan-out**: it already is one in the extension; the gain is Clef first on each row (2 to 4 s a card against 3 minutes for a run). The library covers the per-row and per-card checks that depend only on the card; the fact-derived scopes need `join` and a cross-input `where`.
- Neither side is the baseline: the cases where the 27B's flag was wrong (`crowd_metric_as_value`, the double-counted `$ref`, the `none`-resolver flags, #2185's self-contradicting note) are about as common in what I read as the cases where the fan-out was blind (caps, missing issue text, unselected sentences).

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

Sleeper adds two selectors to the declaration set: `join` (look up a row's facts by key in a second stored input) and a `where` that can compare a field with a joined value. Without them the fact-derived scopes (`differs`, `close`, `known`, `consolidation`, `conflict`) cannot be declared. The facts must be stored with the card, or the extension must hand them over as an input.

The extension provides `inputs` (named texts: report, request, diff, review, guarantees...), names the splitters for its kinds, and may register splitters the library lacks. The judge owns the library of strategies in this spike, the three-valued fold (so a check is decided as soon as the unknown chunks cannot change it, and only then escalates), per-version memoization of model splits, span verification, the `expect` shape check with fallback, and the verdict field recording which splitter ran and why a fallback fired.

One change to the verdict follows. A fold of `all` over N chunks replaces the share-of-units average for these checks, so a check's `met` becomes a decided boolean with the failing chunks as its notes; `share` and `atLeast` keep the fractional form where a check wants one. The library is generic enough that research and review share strategies: `sentences`, `listItems`, `related` (overlap) and the `all` fold run on research asks, summary sentences, PR promises and contract guarantees without a line of kind-specific code. Two checks on one artifact need different strategies in every kind measured: a research report uses `sentences`/`all` for its asks and `digestPairs`/`any` for contradictions; a review uses `sentences`/`all` over description promises and `diffFiles`/`coverageUnion` over changed files.

## What the spike does not show

- **Round 2 limits.** 76 of the 831 judged versions on prod (722 reviews, 49 contracts, 43 Sleeper cards, 17 research reports). Reviews: 27 of 200 rejected versions, chosen because `promises_handled` or a triggered `sibling_parts` was flagged; the other 173 were not run. Sleeper facts are not in the card, so rows were judged on the card alone. The model splitter for the six real requests with no ask is not simulated. The recorded verdicts were made under different rubric versions; only current-rubric versions are in the agreement counts.
- **Timing.** Today's pass is paired to its version by order and matched on count (11 sessions unpaired); slot waits are not logged, so "queue-inflated" is a threshold on seconds per escalation, not a measurement. The fan-out time is Clef-side latency plus code; the ssh pipe adds about 7%. Sleeper's judge pass is not separable from the workflow run.
- **My adjudications are one reader's.** Where I wrote "recorded right" or "fan-out right" I read the artifact and the notes; several rows are "undetermined" because the 27B left no note.

- **No defect-catching claim for reviews or contracts.** Every recorded review approved and every recorded contract spec passed, and the 27B's recorded answers say "nothing missing" for `promises_handled` on all 14. Agreement there means no false block, not a catch. The only labelled catch/false-block data is the 20 research cases (4 seeded-bad for asks, 2 for summary and for contradiction, 5 good).
- **The 27B is not measured.** Its recorded verdicts are a reference, not truth, and the model splitter is simulated by hand-written splits.
- **Small and hand-built.** The research set is short invented pages; its contradictions are single numbers, which the extractor suits. The 14 reviews are the newest approves, three quarters under 25 KB of diff. Four contracts, one JSON Schema.
- **Latency is Clef only,** from inside the container, one request at a time, while prod ran research sessions on the same host. Today's path timings assume 25 s per escalation, not a measured run.
- **Not built:** Clef picking hunks from a menu of headers, per-version memoization, `dtsExports` and `relevanceWindow` on any input (they are in the library and not exercised), and the effect of fan-out on a cost budget with several checks sharing a scope.
- **Token cap:** chunk estimates use the judge's 4-bytes-per-token rule, not Clef's tokenizer.

## Rerunning

`fetch.ts`, `fetch-diffs.ts`, `fetch-contracts.ts`, `fetch-all.ts` (every version of every judged artifact) and `fetch-prs.ts` pull the data read-only into `tools/spike-fanout/data/` (not committed: it holds prod research conversations); `run.ts research|baseline|review|contracts|splitters|prod-stats` asks Clef and writes `out/*.json` (also not committed, with a request cache that makes reruns free); `tables.ts` renders the round 1 tables above. Round 2: `select.ts` picks versions, `round2.ts estimate|run` runs the candidates and compares them with each recorded verdict, `disagree.ts` lists the differences to adjudicate, `times.ts` and `inflation.ts` read today's pass times from the session logs (`extract-times.js` runs inside `quack-v1`, read-only), `parallel.ts` measures concurrency, and `tables2.ts` renders the round 2 tables. `DRY=1` chunks without calling Clef.
