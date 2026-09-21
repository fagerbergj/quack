# Judging: verdicts built from checkable facts (design v2)

Status: proposal under discussion with the owner, 2026-09-21. Nothing built. Supersedes the four-slice sketch in judge-overhaul-prompt.md for grounding and scoring. Item 1 (a cap on judge thinking) is deferred until vLLM enforces a budget under speculative decoding.

## What exists today, verified in the code

- `grounded` is judged by citation presence. config/rubric.md tells the judge it cannot see the retrieval log and must "judge grounding by whether each claim carries an inline citation".
- `cites_sources` is code-owned (internal/vetting/judge.go citationScore). Per cited link it scores fetched 1.00, seen in search 0.75, same host fetched 0.50, same host seen 0.25, neither 0. It proves the URL was retrieved, never that the page supports the claim.
- Every fetched page is stored verbatim as a `web_page` artifact, id derived from the URL, with SourceURL and NodeID lineage. The kind is a system kind: agents cannot write or edit it.
- The ledger records every tool call with its args and result (ledger.ToolCallPayload). Data agents such as the Sleeper ones have evidence too.
- The gate already verifies the judge's quote anchors in code (envelope.go sanitizeAnchors): a quote that is not verbatim in the answer is dropped. This is the mechanism the design extends.
- rubric.yaml already lets a criterion carry its own scale with its own pass mark, and already has code-owned criteria (`deterministic: true` with a `fix`).
- A citation-only failure already gets a lighter revise prompt (citationOnlyFailure).
- Measured on prod (two research chats, 17 judged rounds): 7 failed; 3 on a criterion at zero (grounding twice, a missing synthesis part once), 4 on partial scores, 3 of those on citation form alone. A plain mean would have passed both zero-grounding rounds.

## Principle

A verdict is computed from small facts that code can check. A model is only asked questions that are atomic, local to a short passage we supply, and answered with a verbatim quote. Code verifies the quote exists. No model decides what gets checked.

## Model

1. Units. Code segments the deliverable (the declared artifact when there is one, otherwise the answer) into sentences, list items and table rows. A unit is checkable when it contains a specific (number, percentage, currency, date, quoted string, proper name) or a citation. Specifics are found by code with normalisation, so none can be missed by an extractor.
2. Citation scope. Deterministic pairing: citations inside the unit, else the nearest following one in the same paragraph, list item or row. Numbered markers resolve through the reference list. A references-only block pairs with nothing.
3. Evidence resolvers. A citation or policy resolves to stored text: URL to the `web_page` artifact, path and line to the repo at the reviewed commit, data agents to this run's tool results in the ledger, upstream nodes to their artifacts. One verifier, several resolvers.
4. Checks, in order.
   - Locate (code). Find the normalised specific, or the unit's key terms, in the evidence and cut a window around it. This tier can reject or locate. It can never accept: a figure appearing on a page does not mean the page supports the claim.
   - Verify (model). Given the unit in context and the window: supported, unsupported or cannot tell, with a verbatim quote. Code checks the quote exists in the evidence and, for a specific, contains it. A computed figure is supported when the model names operands that code finds in the evidence and the arithmetic reproduces the value.
   - Second look. An unsupported verdict on a specific is re-checked once with a wider window before it can fail a round, because a false fail costs a revise.
5. States. supported, unsupported, unverifiable (with the reason: page truncated, no stored text, table unreadable), and not checked (the verifier failed). A failed verifier is never a negative verdict.
6. Structured deliverables. For a JSON artifact, grounding is field provenance: each data leaf must equal a value present in this run's tool results. No sentences, no model.
7. Measures and decision. Hard gates: any unsupported specific, any uncited specific where the agent's policy requires citations, an invalid artifact, truncated output, a required part missing. Ratios with a per-criterion pass mark: citation accuracy (cited units whose citation supports them over cited units) and the unverifiable share. The lowest criterion still decides. No averaging.
8. Records. Unit checks are stored as a gate-only system kind keyed by unit hash, evidence hash, verifier version and rubric version. The verdict envelope carries the failing units.
9. Revise. The worker gets the failing units with what was searched for, and may rebut with a quote, which code verifies the same way. Only changed units are re-checked. Whole-answer criteria are re-judged whenever any section changes.

## What the attack changed from v1

Correctness

- v1 let a model extract claims. Missed claims would pass silently. v2 finds units and specifics in code.
- v1 treated a literal match as support. v2 lets the code tier only reject or locate.
- Format variance (1,234 and 1234, 60% and sixty percent, rounding under "about") would cause false fails. v2 normalises and takes a second look before failing.
- Derived figures such as a projection swing are on no page. v2 verifies operands and arithmetic.
- v1 covered web research only. v2 resolves evidence for data agents, code review and synthesis, and checks JSON artifacts by field provenance.
- Truncated or unreadable pages need their own state, not unsupported.
- A supported-over-total ratio can be diluted by padding. Fabrication is gated absolutely; ratios are kept for citation accuracy.
- Whole pages as prompt prefixes would cost hundreds of thousands of prefill tokens per round. v2 sends windows, batches units per page, and must run inside the node's admission slot or small calls will queue behind other chats.

Eval writer experience

- Today a rubric change is proven with a rig cycle of ten to forty minutes; there were about ten this week. `quack eval` re-runs a whole conversation and `experiment run` re-runs the agent. Nothing re-judges a recorded answer under a working-copy rubric. That judge-only replay is the first thing to build: it is the author's fast loop and the proof harness for every later slice.
- Grounding stops being prose each author rewrites. It is a code-owned criterion that appears when applicable, as `cites_sources` does now. A rubric names it only to set its pass mark. Existing rubrics keep working unchanged.
- A failed round shows a table: unit, figure, cited source, state, quote or what was searched for. Replay prints the same table.
- Pass marks come from data: replay prints each measured criterion's score distribution over recorded runs and which rounds a mark would have failed.
- `server validate` lints rubrics offline: a prose criterion that duplicates a code-owned one, bands that do not cover the scale.
- The write-judge-rubric skill says: write prose only for judgment calls.

Abuse, kept proportionate

- The realistic adversaries are a worker under revise pressure and hostile text inside fetched pages.
- Hedging or reframing a figure does not hide it, because code finds figures regardless of phrasing. Deleting an unsupported claim is a legitimate fix while the question is still answered.
- A page cannot inject its way to a pass on a specific: the verifier has no tools, sees a bounded window as data, and must return a quote that exists and contains the figure.
- New record kinds must be system kinds. Unverified observation: recordstore's edit path refuses system kinds only, so gate-only structured kinds such as judge_round may be editable by a worker. Worth a short check when the records are built.

## Decisions needed from the owner

1. Any unsupported figure fails the round after a second look. Zero tolerance, or a count?
2. A true claim attributed to a page that does not say it: hard gate, or the citation accuracy ratio with a mark chosen from data?
3. Uncited figures in research deliverables: keep failing, as the rubric prose says today?
4. Build order: replay tool first, then the new checks in shadow mode (computed and recorded, not gating) until they agree with reality on recorded and live rounds, then switch gating on.

## Proof plan

Replay the 17 recorded rounds. The design must fail the invented FiveThirtyEight figure at the locate tier, pass the three citation-form rounds if their claims are supported, and still fail the round with three weak criteria. Then shadow mode on live research chats, comparing unit verdicts with the current judge, before anything gates.
