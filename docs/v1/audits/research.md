# Audit: research prompt and rubric

- **Agent:** `v1/agents/researcher` (prompt v1, 195 words) and `research.report` rubric v1 (`v1/packages/research/src/report.ts`, one provenance gate)
- **Ported from:** v0 `agents/web-researcher` (prompt 867 words, 8 criteria), `agents/synthesizer` (237 words, 7 criteria) and `agents/orchestrator` (1,424 words; research is one routing line and the clarify rule)
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861))
- **Outcome:** keep the v1 prompt as it is, with three additions to A/B before they land. Port the v0 rubric as four code gates and six weighted model checks. Drop the synthesizer and orchestrator rubrics. The model checks wait on #1857 (source text) and a `request` evidence scope.

## Effectiveness

There is no labelled research set: no dataset for the researcher exists in `v1/packages/eval`, and v0 left no golden set for web-researcher. The v0 evidence is incident and A/B history from git, below. Nothing here measures how often v0's gate caught a bad report.

| Change | Evidence |
| --- | --- |
| Cite only fetched URLs (#170, #171) | One run did 8 good fetches, cited 6 URLs from memory (3 were 404 or a dead domain, 2 were live pages never fetched) and cited none of the 8. The code check scored backing 0.12; the LLM judge rated the answer "perfect". |
| Snippets may back a figure, never contradict one (#1544) | Replaying 17 recorded rounds with snippet evidence took contradicted specifics from 2 to 20, nearly all false (a snippet showed an older draft window than the live page). 46% of cited specifics had no stored page. |
| Judge counts quoted findings (#1537) | On 17 rounds x3 with the 27B judge, `clean_output` swing fell 0.14 to 0.03. `grounded`, `citation_quality` and `internally_consistent` stayed at 0.10-0.15 swing: the judge finds a real defect on some passes and not others. |
| Code owns figure citation (#1538) | The judge's `grounded` swung 0.13-0.15 on the same answer because it recounted citations by hand. |
| First line is the answer (#1758) | Paired A/Bs on 30 and 15 prod runs: turns down 39% and 25%, tool calls down 44% and 24%, gates passed, quality flat. |
| Hard tool-call cap (5a843bed) | qwen3.6 fired 688 tool calls on one node before the cap. |
| Batched search and fetch (#1489) | 234 of 305 prod turns issued exactly two tool calls. |

Nothing in v0 measured `answers_question` or `internally_consistent` beyond three live seed items in `docs/configuration/judge-measurement.md`. Those two criteria have no evidence of catching anything.

## Rubric: criterion by criterion

v0's web-researcher rubric fully overrides `config/rubric.md`, so the default rubric contributed only its band grammar (3 = no finding, 2 = one minor, 1 = two or three, 0 = four or more). The v1 weights replace that grammar. Checks are written in the reviewer rubric's style; scopes need `reportEvidence` to emit them.

| v0 criterion | Decision | v1 check and reason |
| --- | --- | --- |
| `cites_sources` (code) | Port, done | `cites_fetched_sources`, gate. v0's partial credit for search-only URLs (0.25-0.74) is gone because `[sN]` ids exist only for fetches, so a snippet cannot be cited. |
| `specifics_cited` (code) | Port as code gate | `findings_cited`: share of Findings bullets carrying a `[sN]`. v0 detected "figures" by regex and skipped answers under 10 of them; v1's format makes the bullet the unit and needs no heuristic. |
| `specifics_supported` (code + tool-less model read, zero tolerance) | Port as model checks, after #1857 | `supported_by_source`, weight 4, scope `finding`: "Does the fetched text of the source this finding cites state what the finding claims?" and `contradicted_by_source`, weight -4, scope `finding`: "Does the fetched text of the source this finding cites contradict it?" Give the judge the page window around the claim (v0 used evidence windows, then a window three times wider before a contradiction counted), not the 500k-char text. Fetched text only: #1544 shows snippets produce false contradictions. |
| `citation_quality`, placement | Fold into `findings_cited` | A `[sN]` sits on the bullet it supports by construction of the format. |
| `citation_quality`, quoted words appear | Port as code gate | `quotes_exist`: a quoted span in a finding is a substring of the cited source's text (the reviewer's `quotes_exist` pattern; #1857 makes the text available). |
| `citation_quality`, outlet matches host | Drop | v1 cites ids, not outlet names. v0 also called weak sources "a note, not a finding". |
| `grounded` (judge: self-contradiction only) | Port | `no_self_contradiction`, weight -4, scope `review`: "Does any figure, date or name in the report conflict with another passage of the report?" The v0 honest-failed-retrieval cap is moot: a report with no fetched source cannot pass `validateReport`. |
| `answers_question` | Port | `answers_every_ask`, weight 4, scope `request`: "Is every distinct ask and constraint in the question (place, timeframe, budget, audience) answered in the Summary and Findings, or named under Assumptions as unsettled?" Needs the question as an evidence unit. This is the only check that catches a well-sourced answer to the wrong question. |
| `internally_consistent`, conclusions | Port | `summary_follows_findings`, weight 3, scope `review`: "Does every claim in the Summary follow from a Finding?" The contradiction half is `no_self_contradiction`. |
| `clean_output` | Drop | The report is a file validated for its three sections, and the chat reply is one sentence (prompt line 4). The leak v0 fixed (#303) was a chat reply. Revisit if narration shows up inside `report.md`. |
| `synthesizer.complete_synthesis` | Drop | Same question as `answers_every_ask`. The other six synthesizer criteria are copies of the web-researcher's. |
| `orchestrator` (3 criteria) | Drop | There is no router: one session per question (D32). |

Two carry-overs that are not criteria. `grounded` still needs the v0 recency caveat (86833b31: the judge flagged real 2025 shows as hallucinations because its own knowledge was stale), so each model check's instructions say an unfamiliar cited fact is not evidence of error. And v0's date rule (`rubric.yaml` notes: wrong-year searches are a `grounded` failure) has no v1 counterpart; see Missing below.

## Prompt: instruction by instruction

v0 `web-researcher/prompt.md` against v1 `researcher/prompt.md`.

**Still applies, already in v1:**
- Lines 1-3, retrieve before citing. v1 line 1 and step 1 ("fetch the page before you cite it") and the `[sN]` rule cover it, and #170/#171 are why it stays.
- Line 17, disagreeing sources. v1: "Where sources disagree, say so and cite both."
- Line 34, answer first. v1's Summary section is first in the file.
- Line 36, a link per claim. v1's `[sN]` per bullet.
- Line 3's "keep it and say plainly that it's unverified". v1: "listed under Assumptions as unverified".

**Dead, because the harness does the job:**
- Line 3, how the citation gate scores fetched, searched and unseen URLs. v1 has one tier and `submit_report` lists failures.
- Line 5, recalled memory as tradecraft. The researcher has no memory tools.
- Line 7, "a turn that ends with the answer only in reasoning arrives empty". v1 writes a file and calls `submit_report`.
- Lines 16 and `grep_artifacts`/`read_artifact`. v1 has neither; `web_fetch` returns the first 30,000 characters.
- Lines 20 and 5a843bed's cap. `maxTurns` 60 bounds effort structurally.
- Lines 38-50, the Sources `<details>` block. The reading view renders sources from `research.sources` (RFC).

**Missing from v1:**
1. Line 24, primary and authoritative sources for factual claims, and line 28, fetch the original when a source cites another. No measurement; one sentence each, A/B on a research dataset first.
2. Line 20's stop rule ("write the answer with what you have and name the gaps"). The cap bounds cost but a run that hits it delivers nothing. Needs the same A/B.
3. Date awareness (v0 rubric notes). v1 has no date tool or line in the prompt, so a time-sensitive question can be searched under the wrong year. Fix in the harness (inject the date), not the prompt.
4. Line 15, fetch before stating exact prices, addresses or hours. Covered by the fetch-before-cite rule; no addition.
5. Line 30, repository questions. v1 has the coding tools (RES-2), but a cloned file has no `[sN]` id, so `cites_fetched_sources` would fail any claim from a repo. This is a spec gap, not a prompt gap; see Follow-ups.
6. RES-1. The prompt has no clarify step. v0's orchestrator rule (line 13 on) is the tested text: ask only when the ambiguity changes the plan (an entity with several referents, a reference with no antecedent, two readings leading to different work), and otherwise proceed on a default. It belongs in the `research.clarify` decision point, not the researcher prompt.

**Wording:** v1 is 22% of v0's length and each line maps to a tool step or the report format. No cut is warranted.

## Porting decision

1. **Keep the v1 prompt unchanged** until the dataset in Follow-ups exists. Add the three prompt items above (sources, stop rule, repo citation) only if an A/B on that dataset shows them helping; they are untested in v0.
2. **Code gates:** keep `cites_fetched_sources`; add `findings_cited` and `quotes_exist`.
3. **Weighted model checks** (rubric v2, after #1857): `supported_by_source` 4, `contradicted_by_source` -4, `answers_every_ask` 4, `summary_follows_findings` 3, `no_self_contradiction` -4. Threshold stays 0.8.
4. **Dropped:** `citation_quality` outlet matching, `clean_output`, the synthesizer rubric, the orchestrator rubric.

## Follow-ups

- Build a labelled research set (known-good reports, and reports with a seeded unsupported claim, a wrong-year search, an unanswered constraint), replay the checks with `quack judge replay`-style measurement, and report catch and false-block rates before rubric v2 ships. v0's 0.10-0.15 swing on interpretive criteria means the model checks need that proof, not an assumption.
- Decide how a claim from a cloned repository is cited (a source id for a repo file) so RES-2's code sources pass the provenance gate.
- Single-query `web_search` and single-URL `web_fetch` cost round trips v0 removed with batching (#1489); that is a tool change, tracked outside this audit.
