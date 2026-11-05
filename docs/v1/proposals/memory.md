# Proposal: memory, rewritten

- **Status:** proposal for the owner, 2026-11-05. Nothing here is decided until the owner signs it off.
- **Replaces:** [memory requirements](../extensions/memory.md) (MEM-1..MEM-5) and the [memory RFC](../rfc/extensions/memory.md). Amends D7, keeps MEM1-MEM3. The [audit](../audits/memory.md) stands: nothing from v0 is ported.
- **Owner's direction (2026-11-05):** a full rewrite with three jobs (operator facts, subject knowledge, past sessions). The shape: seed at session start, extract candidates at session end, grade them on a schedule into commit, overwrite, revisit or dismiss. The write policy and build route wait on evidence.
- **Evidence:** ten research reports (mem1-mem5 for memory), checked against their sources as listed in [Sources checked](#sources-checked).

## Decisions proposed

| # | Decision | Confidence | Main evidence |
| --- | --- | --- | --- |
| 1 | Past sessions are stored raw as **episodes** written in code from each run's sealed artifacts and approvals. No model summarizes them. | High | Consolidated LLM memory "first rises, then degrades, and can fall below the no-memory baseline", while "an episodic-only control ... remains competitive" (arXiv 2605.12978). Sealed artifacts have already passed the judge. |
| 2 | **Provenance decides who may commit.** The grader commits a fact quoted from the operator or from a file on the subject's default branch. Any other fact needs the operator's approval, and a fact about the operator commits only from the operator's own words. Agents get no write tool. | Medium-high | Add-all memory shows "flat or declining success rates" (arXiv 2505.16067). Query-only injection stores attacker records 98.2% of the time (MINJA, arXiv 2503.03704), and a poisoned page entered memory through session summarization (Unit 42). Quack sessions read PR text, issue comments and web pages written by other people. |
| 3 | **Overwrite supersedes and never deletes.** The old fact keeps its text and gets `invalidAt` and `supersededBy`. Conflicts are settled when the candidate is graded, so the agent is never shown both values as current. | High | Graphiti: "old facts are invalidated — not deleted". Small Qwen3 models (0.6B, 1.7B) "recover only when the conflict is pre-resolved for them" (arXiv 2609.01852). |
| 4 | **Build in stages.** First benchmark Mem0 OSS against a minimal homegrown baseline (episodes plus recall, no extraction). Build the extractor and grader only if the fact probes show a gap that episodes leave. | Medium | MEM3. Mem0 OSS has been ADD-only since April 2026, so it does not perform the overwrite this design needs. The episodic-only result says the cheap arm may be enough. |
| 5 | **Storage is SQLite** in the extension's data directory, with in-process cosine search and FTS5 for keyword search. Qdrant serves only the Mem0 benchmark arm. | Medium | A fresh install runs from `QUACK_DATA_DIR` alone, and external services are opt-in. One operator's store stays in the thousands of rows. |

## Requirements (replace MEM-1..MEM-5)

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| MEM-1 | MUST | Keep three kinds of memory, each scoped: facts about the operator (global), facts about a subject (a repository, a league, a project), and episodes (one per finished run, scoped to its session's subject). | Owner 2026-11-05, MEM1 |
| MEM-2 | MUST | Write an episode for each finished run from its sealed artifacts, verdicts and approvals, without a model call. | Decision 1 |
| MEM-3 | MUST | At the end of each run, extract candidate facts. Each candidate quotes the exact span it rests on, from an operator message, an agent message or a repository file read in the run. A candidate whose span is not found verbatim is dropped. | Decision 2 |
| MEM-4 | MUST | Grade candidates on a schedule and record one decision for each: commit, overwrite, revisit or dismiss. The record includes every check's answer and the reason. | Owner 2026-11-05 |
| MEM-5 | MUST | Let the grader commit or overwrite only a fact quoted from the operator or from a file on the subject's default branch. Any other passing fact waits for the operator's approval. A fact about the operator is stored only from the operator's own words. | Decision 2, amends D7 |
| MEM-6 | MUST | Overwrite by superseding: keep the old fact, marked with when it stopped holding and what replaced it. No decision deletes a fact; only the operator does. | Decision 3 |
| MEM-7 | MUST | At session start, render the current operator facts and the session subject's facts as a stable prompt section under a token cap, in a fixed order. A recall tool searches facts and episodes in the session's scope only. Superseded facts are returned only on request, labelled historical. | MEM1 |
| MEM-8 | MUST | Never store a fact that matches a secret pattern, or a fact about the operator's health, legal, financial or political matters. | v0 bar (audit) |
| MEM-9 | MUST | Let the operator list, edit, delete and restore memories, read every grader decision with its reason, and undo any automatic commit. | D7 |
| MEM-10 | MUST | Run an agent's benchmark with memory off and with each candidate memory system, and keep memory on for an agent only where it wins. | MEM2, MEM3 |

Each row is testable as stated. MEM-3, MEM-5, MEM-6 and MEM-8 are code checks, MEM-4 and MEM-9 are visible in the operator view, and MEM-10 is the benchmark below.

## Design

### Hooks

- **Session start** (lifecycle `created` and `resumed`, D17): the session key gives the subject (D19). `github:owner/repo#123` maps to `github:owner/repo`, a league key maps to its league, and an ad-hoc chat has no subject, so it gets operator facts only. The hook renders the pinned section (MEM-7) and registers `memory_recall`.
- **Run end** (lifecycle `idle`): the hook writes the episode in code (MEM-2) and queues extraction for the entries since the last extraction cursor. Neither step runs in the hot path; the run has already delivered.
- **Grader** (schedules extension, SCH-1): a nightly schedule submits to the session key `memory:grader`. Its model checks go through `@quack/judge` with a `memory.candidate` rubric, so routing is Clef first with escalation to the 27B (D16). It runs at concurrency 1 and leaves the prod slots free (D60).

### Candidate extraction

- **Input:** operator-authored messages, the agent's own messages, and the text of repository files the run read, each with its entry id. Raw tool output from web pages, PR bodies and comments by other people is excluded, so untrusted text cannot become a quoted span. Text relayed through an agent message gets the `other` provenance (below).
- **Chunking:** declared windows of N entries (`chunk: window`), with the fan-in rule `union` deduplicated on normalized text. No bespoke code.
- **Output per candidate:** the kind (operator or subject), one sentence in the third person, the quoted span, and its entry id. Provenance is computed in code, never asked of the model:
  - `operator`: the span sits in a message from a participant with the operator role.
  - `repo`: the span sits in a file at a commit on the subject's default branch, checked with `git show`.
  - `other`: everything else, including the agent's own claims.

### Grader rubric (`memory.candidate`)

Every check runs on one unit: a single candidate, or a single pair of a candidate and an existing fact. None asks the judge to search a whole input for something absent. "Does this contradict anything in memory?" is an open-ended search, so code turns it into a listed set first: the top 3 existing facts in the same scope by cosine similarity, each judged as its own pair. A contradiction outside the top 3 is missed. That is the accepted ceiling, and the update-arc probes measure it.

Checks are written as declarations: `scope` is the unit list, `chunk` is how units are cut, and `fanIn` is how unit answers combine into the decision. This assumes the field names the judge fan-out spike lands and adopts whatever it settles on.

| Check | Criterion (mem3) | Kind | scope / chunk / fanIn | Question or rule |
| --- | --- | --- | --- | --- |
| `span_found` | Confidence | gate, code | candidate / item / all | The quoted span is in the cited entry verbatim. |
| `one_fact` | Specificity | gate, code | candidate / item / all | One sentence, at most 300 characters. |
| `no_secret` | Sensitivity | gate, code | candidate / item / all | No match against the secret patterns (tokens, keys, passwords). |
| `stated` | Confidence (A-MAC "factual confidence") | model | candidate / item / all | Does the quoted span state this fact, rather than only suggest it? |
| `durable` | Type prior (A-MAC's most influential factor) | model | candidate / item / all | Will this still hold a month from now, rather than describing the state of this one session? |
| `reusable` | Utility | model | candidate / item / all | Would knowing this change what an agent does in a later session on this subject? |
| `sensitive_topic` | Sensitivity | model, pitfall | candidate / item / all | Is this fact about the operator's health, legal, financial or political matters? |
| `same_attribute` | Novelty and conflict | model | pair / pair(candidate, top-3) / any | Do these two statements describe the same attribute of the same subject? |
| `same_value` | Conflict | model | pair / pair / per pair | (asked only where `same_attribute` is yes) Do they give the same value for it? |

A stricter bar for values and beliefs comes for free. Category-conditioned retention found "only 77.9% of value and belief assertions are supported by their source, versus 96.2% for all other categories" (Owolabi, NeurIPS 2026). Here an operator value can only be stored from the operator's own words.

### Decision mechanics (code over the answers)

| Answers | Provenance `operator` or `repo` | Provenance `other` |
| --- | --- | --- |
| Any gate fails, `stated`/`durable`/`reusable` is no, or `sensitive_topic` is yes | dismiss | dismiss |
| Passes; no pair has `same_attribute` | commit | revisit |
| Passes; a pair has `same_attribute` and `same_value` | dismiss as duplicate; +1 corroboration on the existing fact | same |
| Passes; a pair has `same_attribute`, different value | overwrite (supersede) | ask the operator |
| Operator-kind candidate | allowed only with `operator` provenance | dismiss |

- **Revisit** holds an `other` candidate for up to 30 days. If a later session yields the same attribute and value (two independent sessions), it goes to the operator as an approval (APR-1). Otherwise it expires as dismissed. This is the corroboration gate that the consolidation result argues for.
- **Overwrite** sets `invalidAt` and `supersededBy` on the old row. An `operator`-provenance fact is overwritten only by another `operator`-provenance candidate.
- No job rewrites or re-summarizes committed facts. The only later changes are corroboration counts and operator edits.

### Retrieval and seeding

- **Pinned section:** current operator facts, then the subject's facts, sorted by id so the prefix cache stays warm (MEM1). It is capped at 1,500 tokens; past the cap, facts are ranked by corroboration count, then age. The header carries the audit's caveat, "verify before relying on it; the repo can change under you", and renders facts as quoted data, not instructions.
- **`memory_recall(query)`:** hybrid search (cosine plus FTS5 BM25, fused by reciprocal rank) over facts and episodes. Scope is filtered in code before ranking, so a fact from one repository never reaches a session on another. Each hit carries its date, provenance and source link.
- **Recent episodes:** the pinned section adds the titles and dates of the last 3 episodes for the subject, about 60 tokens.

### Storage

SQLite in the memory service half's data directory holds four tables:

- `facts`: scope, kind, text, provenance, source entry, span, `validFrom`, `invalidAt`, `supersededBy`, corroborations and embedding.
- `candidates`: status and `revisitUntil`.
- `decisions`: candidate, action, answers, reason and time.
- `episodes`: session key, run, subject, title, artifact refs, time and embedding.

Vectors are compared by brute force in process (`shortcut: linear cosine scan, fine to about 50k rows; move to Qdrant past that`). The embedder is whatever llama-swap serves: v0's config names `qwen3-embed`, and whether it is still deployed is an open question.

## Build route

MEM3 says open source first, and homegrown only if it beats OSS on MEM2's benchmark.

| System | Fit on this host | Verdict |
| --- | --- | --- |
| **Mem0 OSS** (Apache-2.0) | Python library; documents vLLM and Ollama LLMs, an OpenAI embedder with `openai_base_url`, and Qdrant. Since April 2026 extraction is "Single-pass ADD-only ... no UPDATE/DELETE", and "the new fact is stored alongside the old one". Graph memory "is removed from the open-source SDK". | **Benchmark it.** It is the lightest maintained system that runs fully local. It does not overwrite, which the benchmark's update arcs will show. |
| Graphiti (Apache-2.0) | Needs Neo4j 5.26, FalkorDB or Neptune. Warns that "very small models frequently emit JSON that doesn't match the requested schema". | Not now. A graph database is too heavy for one operator. Borrow its invalidate-not-delete rule (decision 3). |
| Letta (Apache-2.0) | A stateful agent runtime whose development moved to `letta-code`; background "dreaming" subagents rewrite memory. | Not a comparator: it replaces quack's agent loop, so the comparison would be confounded. |
| LangMem (MIT) | A library over a LangGraph store; Python. | Not now. It is tied to LangGraph, and no local-model path is documented. |

Mem0's published scores (LoCoMo 92.5, LongMemEval 94.4) "reflect Mem0's managed platform, which includes proprietary optimizations not available in the open-source SDK" (Mem0 README). Its OSS numbers here come only from this benchmark.

**Arms, in build order:**

1. `off`: memory disabled (MEM2's "without").
2. `episodes`: the minimal homegrown baseline, with MEM-2 episodes, MEM-7 recall and no extraction. It costs no model call per run beyond embedding.
3. `mem0`: Mem0 OSS, given each run's operator and agent messages through `add` at run end and `search` at session start. Its retrieved text renders into the same pinned section, under the same cap.
4. `graded`: the full design. Built only if `episodes` and `mem0` both lose fact probes that the design targets.

**Mem0 run requirements on this host:**

- A Python sidecar container with `mem0ai[nlp]` and `fastembed`. Without them "BM25 is silently disabled and search falls back to semantic-only", per the v2-to-v3 migration page.
- LLM: provider `vllm` with `vllm_base_url` set to llama-swap's `/v1`, model `qwen3.8-27b` (the prod model).
- Embedder: provider `openai` with `openai_base_url` set to llama-swap's `/v1`, the same embedding model as the homegrown arm, and `embedding_dims` set to its width. Keep `OPENAI_BASE_URL` unset so it cannot redirect both clients.
- Vector store: Qdrant on the media server, in a dedicated collection per case such as `mem0-bench-<case>`, dropped after the run. No prod collection is touched.
- No hosted API: no OpenAI key is set, so a missed config fails instead of calling out. Mem0's REST server needs Postgres, so the library is used directly.
- Load: one case at a time, started only while `vllm:num_requests_running` is 2 or lower, as the reviewer audit's replay does.

## Benchmark (MEM-10)

**Cases from real sessions.** Take recorded v1 session directories grouped by subject and ordered by time, for example every reviewer and implementer run on `fagerbergj/quack`. A case is a prefix (runs 1..k-1) plus a target run k. To build one, read run k's transcript and list what it had to rediscover that an earlier run already held, such as the test command, a known red or a convention. Then write probes from that list.

| Probe type (LongMemEval's five abilities) | quack memory kind | Example |
| --- | --- | --- |
| Information extraction | operator, subject | "Which command runs v1's tests?" |
| Multi-session reasoning | episodes | "Which PRs on this repo were blocked for env-dependent tests?" |
| Knowledge update | subject, operator | A fact changed between run 3 and run 7; ask for the current value. |
| Temporal reasoning | episodes | "What did the last review of this PR decide?" |
| Abstention | all | Ask about something no run stated; the right answer is "not known". |

Add selective-forgetting probes (MemoryAgentBench's fourth competency) as update arcs, where a superseded value must not be seeded.

**Two levels.**

- **Probes:** cheap and many. For each case and arm, ingest the prefix into a fresh store, then answer each probe with the same reader prompt and model.
- **Task replay:** the MEM2 decision. Replay run k through `node packages/eval/src/main.ts run` with the arm's store snapshot mounted, against the `off` arm. Items live in a `v1-memory` Langfuse dataset (EV1).

**Metrics.**

- Probes: accuracy per probe type; the **wrong-fact rate** (a seeded or recalled fact that is false or superseded); abstention accuracy.
- Task replay: checker pass, turns, input tokens, wall time and revisions (EVL-2).
- Cost: model seconds per run for extraction and grading, latency added at session start, and rows written and dismissed per run.
- Probe answers are short reference strings, scored by exact or substring match where possible. Otherwise one fixed judge prompt applies to all arms, and raw answers are kept for regrading.

**Fairness.** The same model, embedder, reader prompt and token cap apply in every arm, with a fresh store per case and pinned config and dataset revisions.

**Bar.** Start with one case on `fagerbergj/quack` with at least three probes of each type. An arm wins for an agent when task-replay checker pass or turns improve at no higher wall time, with a wrong-fact rate of zero on that case. Widen to EV2's 20 cases only once that holds.

## Failure modes and mitigations

| Failure (mem4) | Evidence | Mitigation | Requirement |
| --- | --- | --- | --- |
| Storing trivia and noise | Add-all memory shows "flat or declining success rates"; agents follow retrieved experience (arXiv 2505.16067) | `durable` and `reusable` checks, revisit with corroboration, pinned cap | MEM-4, MEM-7 |
| Stale fact beats the current one | Small models recover only from pre-resolved conflicts (arXiv 2609.01852) | Supersede at grading; seed only current facts; historical only on request | MEM-6, MEM-7 |
| Consolidation drift | Utility rises, then degrades below no-memory (arXiv 2605.12978) | Episodes kept raw; no re-summarizing job; promotion needs two sessions | MEM-2, MEM-4 |
| Injection and poisoning | MINJA 98.2% injection via queries; Unit 42 poisoned summarization | Spans only from operator, agent or repo files; `other` never auto-commits; facts rendered as data | MEM-3, MEM-5 |
| Sensitive data | v0's bar (audit) | Secret-pattern gate; `sensitive_topic` check | MEM-8 |
| Bleed across subjects | Retrieval matches wording, not scope (mem4; practitioner report, unverified) | Scope filter in code before ranking | MEM-7 |
| Runaway growth | Same add-all study | Dismiss by default; revisit TTL; rows per run reported by the benchmark | MEM-4, MEM-10 |
| Silent wrong memory | v0 "stored and recalled pointless facts" (spec) | Every decision visible with its reason; one-click undo | MEM-9 |

## Open questions for the owner

1. Do you accept decision 2, provenance-based commits without your approval for `operator` and `repo` facts? It amends D7.
2. Should extraction read the agent's own messages at all? They yield the most subject facts, but every one of them is `other` and costs you an approval.
3. How many memory approvals a week are acceptable before the `other` path should simply dismiss instead?
4. Which agent and subject goes first? Reviewer and implementer on `fagerbergj/quack` have the most recorded runs; Sleeper's league is the other candidate.
5. Is an embedding model on llama-swap today (v0 used `qwen3-embed`), or should one be added?
6. May the Mem0 arm create and drop throwaway collections on the media server's Qdrant, and must prod memory stay inside `QUACK_DATA_DIR` (decision 5)?
7. How long are episodes kept: forever, or a set number of days?
8. If Mem0 wins, do you accept its ADD-only behavior (old and new facts side by side, ranked at read time) in place of the overwrite this design specifies?

## Sources checked

Each claim below was opened at its source on 2026-11-05 and is used as stated:

- Mem0 README and `docs.mem0.ai`: the license; the ADD-only algorithm; graph memory removed from OSS; the vLLM, Ollama and Qdrant pages; the embedder's `openai_base_url` and `embedding_dims`; the `[nlp]` and `fastembed` fallback; the REST server needing Postgres; headline scores coming from the managed platform.
- The Mem0 paper's four operations (arXiv 2504.19413).
- Graphiti README and `edge_operations.py`: invalidation that sets `invalid_at` and `expired_at`; the supported databases; the small-model JSON warning.
- Letta: the README's move to `letta-code`; letta.com's sleep-time post; the docs' "Dreaming" page.
- LangMem: the MIT license.
- Papers: LongMemEval (arXiv 2410.10813), MemoryAgentBench (arXiv 2507.05257), arXiv 2605.12978, arXiv 2609.01852, A-MAC (arXiv 2603.04549), arXiv 2505.16067, MINJA (arXiv 2503.03704), category-conditioned retention (NeurIPS 2026 page).
- Unit 42's October 2025 post.

Corrected from the reports:

- mem5 said Mem0's pipeline "explicitly mirrors quack's grader". It does not: current OSS is ADD-only, and the four-operation update phase exists only in the 2025 paper.
- mem5's quote "overwrite an existing memory" is not on the Graphiti page. The page says "old facts are invalidated — not deleted".
- MINJA's title is "Memory Injection Attacks on LLM Agents via Query-Only Interaction", and its rate is 98.2%.
- The category-conditioned paper names no Meta affiliation.
- Letta's current docs call sleep-time compute "Dreaming".
- No Mem0 page says "OpenAI-compatible" for the LLM. Its documented local paths are the vLLM and Ollama providers.

Dropped as unchecked or failed:

- mem2's BEAM figures (48.6%, 26.1%, 16.3%), which failed the judge.
- The LoCoMo score dispute between Mem0 and Zep, the Maximem and MemVerdict audits, and the 6.4% LoCoMo answer-key error.
- MemLeak, TEPA, FadeMem, Memory Worth and the Maximem consolidation pipeline.
- Star counts, release dates and Mem0's latency and token-saving figures.
- Letta's "sleep-time since 0.7.0" date.
