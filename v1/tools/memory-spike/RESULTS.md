# Memory spike: results (one case)

Spike for decision 4 of the [memory proposal](../../../docs/v1/proposals/memory.md) (on `proposals/memory-and-sloppy-code`), with the owner's arm list: `off`, `episodes`, `graphiti`. Mem0 is not an arm this round. Run 2026-11-06 on jaison against llama-swap only (`jaison/qwen3.8-27b`, `jaison/qwen3-embed`), one model call at a time, gated on `vllm:num_requests_running` ≤ 2 (it never waited).

**Verdict, short.** No arm meets the bar. Both memory arms beat `off` on the probes, but both state wrong facts: `episodes` 8 of 22 answers, `graphiti` 2 of 22. On the task replay `episodes` cut turns 28 → 24 and input tokens 961k → 818k with the checker still passing, but wall time rose 216 s → 272 s on one sample each. The bar asks for a zero wrong-fact rate and no added wall time, so neither holds. What the probes do show is below.

## The case

- **Subject:** `github:fagerbergj/quack`, reviewer runs on prod v1. Sessions were copied read-only from the `quack-v1` data volume (`session.sqlite` plus metadata, no workspaces) into `data/` (gitignored). Nothing on prod was written; `GET /export` was avoided because it wakes the session.
- **Target run k:** `github:fagerbergj/quack#2202` (created 2026-11-05 16:08 UTC), "fix(sleeper): bye_coverage is decided by code from the bye holes". It has three requests and 160 turns. Its first run took 41 turns, 1.46M input tokens and 793 s.
- **Prefix (runs 1..k-1):** the 30 sessions created before it on the same subject, #2169 through #2201 (2026-11-04 09:56 to 2026-11-05 15:31 UTC), in creation order. That is 29 reviewer runs and one implementer run (#2117), about 83k tokens of episode text. #2203 started two minutes after #2202 and is left out.
- **Why this target:** its transcript re-derives things the prefix already held:
  - It first reads `agents/sleeper-waivers/prompt.md` (the v0 path), which fails, then finds `v1/agents/…` with `find`.
  - It checks `node --version` and `ls v1/node_modules`, then runs `npm ci` before the tests.
  - It runs v0's Go linters (`go run ./tools/sloplint diff`, which fails on usage, then `go run ./tools/promptlint diff origin/v1`). Most prefix reviews ran v1's `node tools/sloplint.ts diff origin/v1`.
  - It works out the `version.lock` hash from scratch: `grep -rn version.lock`, `cat v1/tools/bundle-locks.ts`, `sed` on `process-driver.ts`, then a one-off digest script. #2200 had done exactly this 45 minutes earlier and #2182 had checked the lock against `bundleLocks()`.
  - The PR concerns `bye_coverage`, whose instability #2198 and #2201 had already reported.
- **Episodes** are built in code (`episodes.ts`), with no model call, one per session: every request text, every sealed artifact version the judge passed (`verdict.passed !== false`), and the final answer. Judge-rejected drafts are left out, since the proposal's episodes rest on artifacts that "have already passed the judge".

## Arms and fairness

- **The same in every arm:** reader model and prompt (`run.ts`; thinking off, temperature 0), memory cap of 2,000 tokens (estimated as chars/4), embedder, and a fresh store for the case.
- **Rendering:** recalled items are picked by rank, then rendered oldest first, each with its date.
- **`off`:** an empty memory section.
- **`episodes`** (`store.ts`):
  - Each episode is cut at its sections and packed into passages of about 1,200 chars, each headed with its PR, date, title and section.
  - Passages go into SQLite (`node:sqlite`, in memory) with FTS5. Recall fuses FTS5 BM25 (top 30) with an in-process cosine scan (top 30) by reciprocal rank (k = 60), then packs passages up to the cap.
  - No extraction and no grader.
- **`graphiti`** (`graphiti/ingest.py`):
  - graphiti-core 0.31.2 on embedded FalkorDB Lite (falkordblite 0.11.0) in a gitignored venv.
  - `OpenAIGenericClient` (`json_schema` structured output, which vLLM enforces) and `OpenAIEmbedder` (width 2,560), both pinned to llama-swap.
  - The placeholder key is `local-no-key`, `OPENAI_*` env vars are removed, telemetry is off, and `max_coroutines=1` with `SEMAPHORE_LIMIT=1`.
  - Each prefix episode goes through `add_episode` (source `text`, `reference_time` = session created, one `group_id`). Each probe runs `graphiti.search(q, num_results=40)`, its default hybrid RRF search with no reranker call. Facts render as `- fact (valid …, invalid …)` up to the cap.
  - Graphiti runs at its own default temperature (1). See "What failed".
- **Scoring:**
  - Exact or substring match where the answer is a string or a set of PR numbers.
  - Otherwise one fixed judge prompt (`JUDGE` in `run.ts`) on the same 27B, used for 4 probes in every arm.
  - Abstention means "not known" or similar in a short answer. A **wrong fact** is an answer that did not abstain and is not correct; an incomplete list counts. Raw answers and every recalled section are in `results/<arm>.json`.
  - ie6's code check (contains `rmc`) is lenient: it passes the `episodes` answer, which says the memory gives no reason. Scored strictly, `episodes` is 4/6 on information extraction and has 9 wrong facts.

## Probe results

22 probes: 6 information extraction, 4 multi-session reasoning, 4 knowledge update, 4 temporal reasoning, 4 abstention. They are defined in `probes.json`, and each reference answer was checked against the prefix text.

| Probe type | off | episodes | graphiti |
| --- | --- | --- | --- |
| information extraction | 0/6 | 5/6 | 3/6 |
| multi-session reasoning | 0/4 | 0/4 | 0/4 |
| knowledge update | 0/4 | 2/4 | 3/4 |
| temporal reasoning | 0/4 | 2/4 | 1/4 |
| abstention | 4/4 | 4/4 | 4/4 |
| **all** | 4/22 | 13/22 | 11/22 |
| wrong-fact answers (answered, and wrong) | 0/22 | 8/22 | 2/22 |
| abstained on an answerable probe | 18/18 | 1/18 | 9/18 |
| reader+judge calls, seconds | 22, 3 s | 26, 15 s | 23, 15 s |
| ingestion seconds | 0 | 307 | 2392 |

- **Wrong-fact rate:** `episodes` 8/22 (9/22 strict), `graphiti` 2/22, `off` 0/22.
- **Abstention accuracy:** every arm abstains on all 4 unanswerable probes, including "what did #2202's review decide", so no target leakage. On answerable probes `graphiti` abstains 9 times and `episodes` once.
- **Where each arm went wrong:**
  - `episodes`' wrong answers are mostly retrieval misses that the reader filled in. For tr2 the 2,000-token section held #2196's v2, v3 and final answer but not its v1 (`request_changes`), so the reader said "first verdict approve". For ku4 it held #2192's 759 tests but not #2198's 767. For ku3 it held only #2182's original string shape.
  - ms3 is a reader conflation: the passages hold both the author's "3 rmc tests fail locally" and the reviewer's "did not reproduce here", and the reader merged them into "they failed in the reviewers' sandboxes".
  - `graphiti` is more conservative. On knowledge update it is the better arm (3/4), because it invalidated the right facts. It set `invalid_at` on "#2182 introduces issueBase …", on "Clef is planned to move to … -np 4", and on the `ClassifierRef.concurrency` fact when #2180 reverted it, and the reader then gave the current value. The fourth invalidation is wrong: "Clef runs on the media box using llama-server" was also invalidated at #2180, though it still holds.
  - Its losses are facts never extracted, or not ranked into 40 results: `npm ci`, the full-suite command, request_changes verdicts, test counts and bye_coverage. Its two wrong facts are an incomplete list (ms2) and #2184 named as the latest issueBase change instead of #2186 (tr4). `episodes` made the same tr4 mistake.

## Cost

| | `off` | `episodes` | `graphiti` |
| --- | --- | --- | --- |
| Ingestion model calls | 0 | 25 embed batches (389 passages) | 395 LLM + 98 embed |
| Ingestion seconds | 0 | 307 (the CPU embedder) | 2,392 (2,235 in LLM calls) |
| Recall per probe | none | 1 embed + FTS5 + a 389-row scan | 1 embed, no LLM |
| Reader + judge calls (seconds) | 22 (3 s) | 26 (15 s) | 23 (15 s) |
| Rows stored | 0 | 389 passages | ≥ 214 entities, ≥ 195 facts, 4 invalidated (see below) |

On the Graphiti side, ingestion per episode ranged from 5.6 s to 347 s and from 2 to 41 LLM calls. Five of the six slowest episodes (#2178, #2197, #2192, #2177, #2173) each contain one runaway generation of about 165 to 275 s (see below).

## Task replay (run k, first request only)

- **Harness:** `replay.ts` drives v1's eval library (`replay`, `itemFrom`, `scoresOf` from `@quack/eval`) on the recorded #2202 session with the github eval double. It does not use `main.ts run`, because that needs a Langfuse dataset and no Langfuse credentials are set here. The scores are the same `scoresOf` numbers the eval CLI publishes.
- **Snapshot mount:** a memory snapshot can be mounted without building the extension, as a copy of the reviewer bundle (v9) whose `prompt.md` ends with a `<memory>` section. For `episodes` that section is recalled once, under the same 2,000-token cap, with #2202's first request text as the query.
- **What it cannot mount:** the `memory_recall` tool, which needs the extension. So this tests MEM-8's pinned section only, not recall during the run.
- **Graphiti:** not replayed. Its probe score is below `episodes`, and the brief replays the arm ahead.

| | `off` | `episodes` |
| --- | --- | --- |
| Checker passed | 1 | 1 |
| Checker score | 0.895 | 0.917 |
| Revisions | 1 | 1 |
| Turns | 28 | 24 |
| Input tokens | 961,131 | 817,712 |
| Output tokens | 15,447 | 17,792 |
| Wall seconds | 216 | 272 |
| Verdict | approve, 1 nit | approve, notes only |

- **What the pinned section held:** passages from #2198, #2199 and #2201, the bye_coverage and judge-feedback context. The `episodes` review cites #2198 ("in line with #2198"). The section did not hold the procedural facts the original run re-derived (`npm ci`, v1 sloplint, the `version.lock` digest), because a PR-description query does not retrieve them. Both replays ran `node --test tools/bundle-locks.test.ts` on their own.
- **Replay caveats:**
  - Each arm ran once, so the 4-turn and 56-second differences are inside what one rerun of the same arm could move.
  - The eval double serves the pin recorded last (head 3502b335, after the author's fixes), so both arms reviewed the fixed head and approve is right. The original first run reviewed d7d318df and requested changes.
  - The replay deletes its session directory, so tool-call transcripts for the two arms were not kept. Only the sealed reviews are in `results/replay-*.json`.

## What failed, and why

**Graphiti setup (about 1.5 hours of effort, inside the 2-hour box).**

- **No `ensurepip` or `uv` on jaison.** The venv was made with `--without-pip`, and pip came from PyPA's `get-pip.py`.
- **A missing dependency.** `graphiti-core[falkordb]` 0.31.2 did not pull in `httpx`, so the first import failed with `ModuleNotFoundError: No module named 'httpx'`. It was installed by hand.
- **Temperature 0 is unusable with this model.** The first attempt ran at temperature 0. Its first call, entity extraction on #2169 (6 KB), generated for about 8.5 minutes, ended with `finish_reason=length` at 16,384 tokens, and failed with `Expecting ':' delimiter: line 6 column 19 (char 108)`.
  - Graphiti retries a `JSONDecodeError` up to 4 times, and at temperature 0 each retry replays the same greedy loop, so that run was stopped.
  - The arm then ran at Graphiti's own default temperature (1, from `LLMConfig`).
  - The reader stays at 0. It decodes short free text, where no loop appeared.
- **Persistence is unreliable.** The last five episodes (#2197 to #2201) are missing from the saved FalkorDB file, though `add_episode` returned for each and the in-process search used their facts (173 recalled facts are dated within those runs).
  - redislite warns `coroutine 'AsyncManagementCommands.shutdown' was never awaited` on close, so the final writes were likely never saved.
  - The entity and fact counts above come from the saved file, so they are lower bounds over 24 episodes.

**Graphiti ingestion at temperature 1** (`results/graphiti-ingest.json`, `results/graphiti-run.log`):

- **Runaway generations:** 5 of 395 LLM calls (1.3%) ran to the 16,384-token cap and returned malformed JSON. They cost 941 s, 42% of all ingestion LLM time.

| Episode | Prompt | What the model emitted | Outcome |
| --- | --- | --- | --- |
| #2173 | entity resolution | `"duplicate_candidate_id":-17231723172317231723…` (16,354 digits) | `ValueError: Exceeds the limit (4300 digits) for integer string conversion`. This is not a `JSONDecodeError`, so there was no retry and **the episode was lost** |
| #2177 | edge extraction | valid edges, then unterminated | `Expecting ',' delimiter: line 8 column 16331 (char 16604)`; the retry succeeded |
| #2178 | entity extraction | `"episode_indices"` followed by about 16k tokens of whitespace | malformed; the retry succeeded |
| #2192 | entity extraction | the same whitespace run | malformed; the retry succeeded |
| #2197 | entity extraction | the same whitespace run | malformed; the retry succeeded |

- **Episodes:** 29 of 30 were ingested; 1 was lost (#2173). That loss rate (3%) is the exact form of the small-model JSON failure Graphiti warns about.
- **What constrained decoding does not stop:** vLLM's `json_schema` mode keeps the output syntactically on schema, yet it still allows unbounded whitespace and unbounded integers, and the 27B fell into both.

**`episodes`:** nothing failed. The 2,000-token cap is the binding limit. Every section filled it (1,941 to 2,011 estimated tokens; the passage separators are not counted), and most wrong answers trace to the deciding passage falling just outside it.

**Not done:**

- Graphiti was not replayed (see above).
- The replay could not exercise `memory_recall`.
- Only one sample per replay arm was run.
- No `v1-memory` Langfuse dataset was created.

## Verdict against the bar

The bar: "an arm helps when task-replay checker pass or turns improve with no added wall time and a wrong-fact rate of zero".

- **`episodes` fails the bar.** Turns improved (28 → 24), but wall time rose (216 s → 272 s), and 8 of 22 probe answers stated a wrong or incomplete fact. With n = 1 per arm the turn and time differences are not evidence either way.
- **`graphiti` fails the bar on wrong facts** (2/22) and was not replayed. It costs about 8 times `episodes`' ingestion time, with 395 calls on the shared 27B against none, and it lost an episode outright.
- **`off` is the baseline:** it abstains on everything and states no wrong facts.

What the probes alone show, without claiming more than one case supports:

1. **Raw episodes recover subject knowledge** that the target re-derived: the v1 sloplint command, the full-suite command, `bundle-locks.ts`, the `v1/agents/` path and the judge revert. `episodes` answers 5 of 6 extraction probes from memory where `off` answers none. This is the gap #2202 actually paid for.
2. **Episodes are weak at "the latest value"** and at counting across runs. They answer 0 of 4 multi-session probes and get 2 of 4 knowledge-update probes. When they miss, the reader answers from a stale or partial passage instead of abstaining, which is the stale-fact failure mode the proposal's MEM-7 is meant to prevent.
3. **Graphiti's invalidation is what helps it on updates** (3/4). It is also wrong once, invalidating a fact that still holds. It abstains on half the answerable probes, so it has fewer wrong facts largely because it answers less.
4. **The pinned section helps with context, not procedure.** It carried relevant context into the replay, but a section recalled once on the task text did not carry the procedural facts. Those come back only through a query about the procedure, which is what `memory_recall` would make. A replay of that needs the extension.

Next step, if the owner wants one: widen to more cases before choosing, and give the reader a recall tool, which means building the extension's recall half. Episodes win on cost (no LLM per run) and on extraction; Graphiti wins on updates and caution. A grader that supersedes facts (the `graded` arm) targets exactly the probes that `episodes` loses.

## Reproduce

From `v1/`:

```
node tools/memory-spike/episodes.ts tools/memory-spike/data/sessions > tools/memory-spike/data/episodes-all.jsonl
node tools/memory-spike/run.ts off
node tools/memory-spike/run.ts episodes
tools/memory-spike/graphiti/.venv/bin/python tools/memory-spike/graphiti/ingest.py all && node tools/memory-spike/run.ts graphiti
QUACK_LOCAL_API_KEY=local-no-key QUACK_CLEF_API_KEY=local-no-key node tools/memory-spike/replay.ts off tools/memory-spike/deployment.json
QUACK_LOCAL_API_KEY=local-no-key QUACK_CLEF_API_KEY=local-no-key node tools/memory-spike/replay.ts episodes tools/memory-spike/deployment.json
node tools/memory-spike/report.ts
```

`data/sessions/` holds the copied prod session stores, one directory per `sha256(key)[:32]`, without workspaces. `dump.ts <dir>` prints one as text.

## Every probe, every arm

**ie1** (information extraction) Which command runs v1's sloplint over a pull request's diff against the v1 branch?  
Reference: From v1/: node tools/sloplint.ts diff origin/v1 (or npm run sloplint -- diff origin/v1).

- `off` abstained (code): not known
- `episodes` correct (code): The command is `node tools/sloplint.ts diff origin/v1`.
- `graphiti` correct (code): The command is `node tools/sloplint.ts diff origin/v1`.

**ie2** (information extraction) In a fresh checkout, what must be run in v1/ before its tests can run?  
Reference: npm ci

- `off` abstained (code): not known
- `episodes` abstained (code): not known
- `graphiti` abstained (code): not known

**ie3** (information extraction) What is the exact command for the full v1 node test suite?  
Reference: node --test --test-force-exit --test-timeout=30000 'packages/*/test/*.test.ts' 'tools/*.test.ts' (run from v1/)

- `off` abstained (code): not known
- `episodes` correct (code): The exact command for the full v1 node test suite is `node --test --test-force-exit --test-timeout=30000 'packages/*/test/*.test.ts' 'tools/*.test.ts'`.
- `graphiti` abstained (code): not known

**ie4** (information extraction) Which file computes what each agent bundle's version.lock should contain?  
Reference: v1/tools/bundle-locks.ts (bundleLocks(), using digest() from v1/packages/distributor/src/process-driver.ts)

- `off` abstained (code): not known
- `episodes` correct (code): The file that computes what each agent bundle's version.lock should contain is `v1/tools/bundle-locks.ts` (specifically its `bundleLocks()` function), which uses the digest algorithm defined in `v1/packages/distributor/src/process-driver.ts:75-84`.
- `graphiti` correct (code): The file v1/tools/bundle-locks.ts computes what each agent bundle's version.lock should contain, as its bundleLocks() function writes the version.lock file.

**ie5** (information extraction) What is the repository path of the sleeper-history agent's prompt?  
Reference: v1/agents/sleeper-history/prompt.md

- `off` abstained (code): not known
- `episodes` correct (code): The sleeper-history agent's prompt is located at `v1/agents/sleeper-history/prompt.md`.
- `graphiti` correct (code): The sleeper-history agent's prompt is located at v1/agents/sleeper-history/prompt.md.

**ie6** (information extraction) Which tests fail in the full v1 suite on the PR author's machine, and why?  
Reference: The three reMarkable rmc render tests; the rmc Python module (binary) is missing on the author's laptop. CI has it.

- `off` abstained (code): not known
- `episodes` correct (code): The PR author noted that "3 rmc tests fail locally," but the reviewer could not reproduce those failures on their own machine (759 tests, 754 pass, 0 fail, 5 skipped). The memory does not specify the reason for the author's local failures.
- `graphiti` abstained (code): not known

**ms1** (multi-session reasoning) Which pull requests in these reviews received a request_changes verdict at least once?  
Reference: #2177, #2185, #2187 and #2196

- `off` abstained (code): not known
- `episodes` **wrong fact** (code): Pull requests #2187 and #2196 received a request_changes verdict at least once.
- `graphiti` abstained (code): not known

**ms2** (multi-session reasoning) Which pull requests changed the extensions.github.settings.issueBase setting?  
Reference: #2182 (added it), #2184 (per-repository map) and #2186 (case-insensitive lookup)

- `off` abstained (code): not known
- `episodes` **wrong fact** (code): Pull request #2182 (feat(v1): extensions.github.settings.issueBase sets the branch /implement cuts from) introduced the `issueBase` setting.
- `graphiti` **wrong fact** (code): Pull requests fagerbergj/quack#2182 and fagerbergj/quack#2184 changed the issueBase setting, with #2182 introducing it as a single branch string and #2184 changing it to a map from "owner/repo" to branch name.

**ms3** (multi-session reasoning) When reviewers re-ran the full v1 suite in their own sandbox, did the reMarkable rmc tests fail there?  
Reference: No. In the reviewer sandbox they skip rather than fail (5 skipped, 0 fail); the author's local failures did not reproduce.

- `off` abstained (code): not known
- `episodes` **wrong fact** (judge): Yes, the reMarkable rmc tests failed in the reviewers' sandboxes because the required Python `rmc` module was missing locally (e.g., 3 failures in the full v1 suite noted in #2198, and 3 rmc failures in #2177).
- `graphiti` abstained (code): Not known.

**ms4** (multi-session reasoning) What did earlier sessions report about how the Sleeper waivers bye_coverage check behaves?  
Reference: It fired on 6 of 7 judged waivers cards (#2198) and swings between 0 and 0.67 on the same card under the same question (#2201).

- `off` abstained (code): not known
- `episodes` **wrong fact** (judge): Earlier sessions reported that the `bye_coverage` check swings between 0 and 0.67 on the same card under the same question, and this inconsistency was left as a separate finding.
- `graphiti` abstained (code): not known

**ku1** (knowledge update) Does the v1 judge currently send every unit to Clef at once (up to the classifier's concurrency), or classify units one at a time?  
Reference: One at a time: #2178 sent them at once, and #2180 reverted it.

- `off` abstained (code): not known
- `episodes` correct (code): The parallel pre-pass was reverted in PR #2180 ("revert(v1): the judge classifies units one at a time again"), so the v1 judge currently classifies units one at a time.
- `graphiti` correct (code): The v1 judge currently classifies units one at a time, because fagerbergj/quack#2180 reverted the parallel classification (from #2178) after measurements showed Clef was compute-bound. The v1/packages/judge directory was restored to its pre-#2178 state, and the classifier.concurrency field was dropped.

**ku2** (knowledge update) How many parallel slots (-np) does Clef's llama-server run with now?  
Reference: 1 (-np 1 -c 8192); #2178 planned -np 4 and #2180 set it back.

- `off` abstained (code): not known
- `episodes` correct (code): Clef's llama-server runs with `-np 1` now, as the revert in #2180 moved it back from the planned `-np 4`.
- `graphiti` correct (code): Clef's llama-server now runs with -np 1, as it was reverted back to -np 1 -c 8192 in home-server following fagerbergj/quack#2180.

**ku3** (knowledge update) What shape does the extensions.github.settings.issueBase setting have now?  
Reference: A map from owner/repo to a branch, looked up case-insensitively; the original single-branch string shape (#2182) is refused since #2184.

- `off` abstained (code): not known
- `episodes` **wrong fact** (judge): The `issueBase` setting is validated as a non-empty string.
- `graphiti` correct (judge): The extensions.github.settings.issueBase setting is now a map from "owner/repo" to branch name, as changed in fagerbergj/quack#2184.

**ku4** (knowledge update) How many tests did the full v1 node suite have in the most recent run that reported a count?  
Reference: 767 tests (762 pass, 0 fail, 5 skipped), in the #2198 review.

- `off` abstained (code): not known
- `episodes` **wrong fact** (code): The most recent run that reported a count for the full v1 node suite was in #2192 (2026-11-05T05:09Z), which had 759 tests.
- `graphiti` abstained (code): not known

**tr1** (temporal reasoning) What did the last review of pull request #2200 decide?  
Reference: approve

- `off` abstained (code): not known
- `episodes` correct (code): The last review of pull request #2200 decided to approve it, noting 0 blocking issues and 1 nit regarding stale pre-v2 wording at `docs/v1/audits/sleeper.md:171`.
- `graphiti` abstained (code): not known

**tr2** (temporal reasoning) What was the first verdict on pull request #2196, and the last?  
Reference: First request_changes, last approve.

- `off` abstained (code): not known
- `episodes` **wrong fact** (judge): The first verdict on pull request #2196 was **approve** (with two nits), and the last verdict was also **approve** (head 23fa5fc4 unchanged, CI now green, 0 blocking, 2 nits).
- `graphiti` abstained (code): The memory does not contain any review verdicts for pull request #2196.

**tr3** (temporal reasoning) Which pull request, reviewed after #2178 and #2179, reverted the change they made?  
Reference: #2180

- `off` abstained (code): not known
- `episodes` correct (code): PR #2180, titled "revert(v1): the judge classifies units one at a time again," reverted the changes made by #2178 and #2179.
- `graphiti` correct (code): Pull request fagerbergj/quack#2180 reverted the changes made by #2178 and #2179, restoring judge/src/index.ts, judge/src/resolve.ts, and judge/test/judge.test.ts to their state at commit 675cbccc.

**tr4** (temporal reasoning) Which was the most recent change to issueBase, and what did it do?  
Reference: #2186: the issueBase lookup ignores repository case.

- `off` abstained (code): not known
- `episodes` **wrong fact** (code): The most recent change was PR #2184, which changed `issueBase` from a single branch string to a map of `{"owner/repo": "branch"}` so that each repository can use a different base branch, with unlisted repositories falling back to their default branch.
- `graphiti` **wrong fact** (code): The most recent change to issueBase was in fagerbergj/quack#2184, which changed the setting from a single branch string to a map from "owner/repo" to branch name. This allowed per-repository base branch configuration, with updates to service.ts to look up the repository in the map and settings.ts to validate the new map structure.

**ab1** (abstention) What verdict did the review of pull request #2202 reach?  
Reference: not known

- `off` correct (code): not known
- `episodes` correct (code): not known
- `graphiti` correct (code): not known

**ab2** (abstention) Which license is fagerbergj/quack released under?  
Reference: not known

- `off` correct (code): not known
- `episodes` correct (code): not known
- `graphiti` correct (code): not known

**ab3** (abstention) Which eslint or prettier configuration does v1 use?  
Reference: not known

- `off` correct (code): not known
- `episodes` correct (code): not known
- `graphiti` correct (code): not known

**ab4** (abstention) Which command deploys v1 to the production server?  
Reference: not known

- `off` correct (code): not known
- `episodes` correct (code): not known
- `graphiti` correct (code): not known

