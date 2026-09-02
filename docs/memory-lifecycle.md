# Memory audit + consolidation lifecycle

Design for quack issue #849 (`memory: answer-mined commits have no truth gate — a confidently false mechanism poisoned the coding bucket`) and the mechanism it forces: memory stops trusting gate-pass as proof of truth and starts trusting what actually happened to the work a memory was minted from.

## 1. Problem

A `ci_fix` run invented a false mechanism (`deadcode flags functions whose callers live in external repositories`) to justify a suppression hack; the judge scored the stated premise 1.0 because a judge scores prose plausibility, not a claimed mechanism against ground truth; the gate-passed answer's mined commit wrote four variants of the bad advice into `role:coding`, where the next reviewer run recalled and repeated it, and a human had to `DELETE /api/v1/memories` by hand.

Correcting insight from the issue's follow-up comment: **the poisoning run passed its gate** (judge 1.0, clean delivery), so provenance gating on gate-pass would not have caught this case, and two of the four poisoned memories were operationally *true* (a dummy init file genuinely does silence that deadcode check) — the poison was a false rationale attached to a rejected practice, which no truth-oracle catches. Every signal that actually falsified the advice was downstream and machine-visible: the PR closed, the branch got force-pushed away, and a human corrected it in a comment. Outcome feedback, not truth-checking, is the oracle this design builds around.

## 2. Design principles

- **No model training.** We adopt Memory-R1's (arXiv 2508.19828) operation taxonomy — ADD/UPDATE/DELETE/NOOP judged by task outcomes — but the outcome grounding is a prompt change to the existing consolidation model (`stores.default_vector.consolidation`), not RL training. Quack's deterministic outcome events (PR merged/closed/force-pushed) substitute for Memory-R1's learned reward signal.
- **Every gate names a deterministic oracle** (the no-flaky-gates rule). Nothing here re-litigates whether a memory's *content* is true — that's exactly the trap #849 fell into. The oracle is always an external, machine-visible event: merged, closed-unmerged, force-pushed-away, human-deleted.
- **Invalidate, don't delete** (Zep/Graphiti, arXiv 2501.13956's bi-temporal model). A memory gets `valid_from`/`invalidated_at`/`invalidation_reason`; nothing is silently destroyed. Recall filters to currently-valid; the invalidated row and its reason stay queryable.
- **Consolidation stays off the hot path** (SSGM's governance checklist, arXiv 2603.11768: consistency verification before storage, temporal decay, evolution decoupled from execution). The per-commit reconcile is already a fire-and-forget goroutine (`commitMemoryOnPass`); the new periodic pass is a scheduled background job in the same spirit as Letta's sleep-time compute / generative-agents' reflection — never inline in a run.
- **Recall tells the agent what it's getting.** New memories are `unverified (single run)`; reinforced ones say how many times. Same design language as the citation-tier legend #822 added to `citeReason` — a compact, once-per-block plain-string legend, not a hidden score.

## 3. Data model

Both backends already carry parallel per-point structs — `qdrantIndex`'s payload map (`payloadContent` etc., qdrant.go) and `memoryRow`'s GORM columns (sqlite.go) — so new fields are additive on both, no migration tooling needed (qdrant payloads are schemaless; sqlite's `AutoMigrate` in `ensure()` already runs on every open).

**Provenance** (stamped once, at write time):
- `chat_id`, `node_id` — which run minted this memory. Already available at every commit call site (`cfg.ChatID`/`nodeID` in vetting/node.go, the tool call context in commit_memory.go).
- `source` — extension name (`"github"`) or empty for a native quack run.
- `minted_at` — redundant with the existing `timestamp` field only in the sense that `timestamp` is "last touched"; `minted_at` never changes after ADD, `timestamp` still updates on UPDATE.

**Lifecycle**:
- `status`: `unverified | reinforced | invalidated`.
- `valid_from`, `invalidated_at`, `invalidation_reason`.
- `reinforcement_count`: increments on each positive outcome event.

**`memory_ops` audit table** (mem0-style history trail): append-only, **Postgres-only**, in `default_postgres` alongside the session/DAG/chat tables `internal/store` already owns — not in qdrant/sqlite, because it's an operational log, not a retrieval corpus, and it needs to survive a memory point's own deletion. Row: `id, memory_id, op (add|update|delete|reinforce|invalidate), actor (consolidator|outcome-feedback|human|run), reason, timestamp`. Every state transition writes one row, including the initial ADD.

## 4. Lifecycle mechanics

**(a) Commit path — shape unchanged.** `Store.Commit` → `commitTo` → `decide` → `apply` (commit.go) keeps its ADD/UPDATE/DELETE/NOOP contract and its neighbour-reconcile call to the consolidation model verbatim. The only change: `apply()` stamps provenance on every write and sets `status=unverified, reinforcement_count=0, valid_from=now` on ADD. An UPDATE keeps the existing memory's status/count — a corrected memory doesn't lose earned trust because its wording changed. The consolidator's `op` struct gains a `Reason` field so a DELETE (→ invalidate, see below) carries *why*, not just an id.

**(b) Outcome feedback — the deterministic oracle.** Events map to minted memories through `provenance.chat_id`:
- **PR merged** → `reinforced`, `reinforcement_count += 1`, for every memory whose `chat_id` matches. Handled today in the GitHub extension's `handlePullRequest` `closed`+`Merged` branch (webhook.go, next to the existing `refreshChatOrigin` call).
- **PR closed unmerged** → `invalidated`, reason `"pr closed unmerged"`. Same branch, `!Merged` case.
- **Head force-pushed away** → `invalidated`, reason `"head rewritten after this run's commits"`. **New work**: the GitHub extension today subscribes to `issue_comment`, `pull_request` (opened/labeled/closed/reopened), `pull_request_review`, `issues`, `workflow_run` — no `synchronize`. This needs a new case in `handlePullRequest` for `action == "synchronize"`, comparing the new head SHA against the SHA quack last pushed for that chat (the `PushedSHA` already threaded through `DeliveryContext`) via a compare/ancestry check.
- **Human DELETE via the memory API** → `invalidated`, `actor=human`, reason from the request (default `"manual delete"`). `DeleteMemory` (internal/server/rest/memory.go) stops calling `store.Forget`'s real removal directly and calls the new invalidate path instead.

**(c) Background consolidation pass.** New, separate from the per-commit reconcile: a periodic job (`stores.default_vector.consolidation.schedule`, a standard 5-field cron string, `""` disables, default `"0 2 * * *"` — daily at 02:00; never runs at boot, issue #961) that, per bucket, clusters memories by `chat_id` + temporal proximity and calls the same `decide()`/`apply()` machinery with a dedupe-flavored prompt variant — collapsing the four #849 variants into one `ADD` + three `DELETE`(→invalidate, reason `"duplicate of <id>"`) had outcome feedback not already invalidated them first. Reuses the existing op taxonomy; only the trigger (cron, not commit) and candidate set (existing unverified memories, not this run's staged candidates) differ.

**(d) Recall.** `Store.recall` (store.go) filters to `status != invalidated` — applied IN THE BACKEND QUERY (qdrant payload filter on the search; sqlite `WHERE`), not Go post-query: a post-filter lets invalidated points crowd valid ones out of the top-k candidate set before the filter runs. The same exclusion applies to the per-commit reconcile's neighbour candidate set, or the consolidator NOOPs a legitimate re-add of corrected advice against an invalidated point. The existing debug log keeps observability by logging the backend's valid-only count. Each recalled memory's text is prefixed with its tier: `[unverified, single run]` or `[reinforced ×N]`, in the same compact plain-string convention as `citeReasonLegend` (#822). This rides the existing injection point — `rec` prepended to the worker prompt in `RunGatedRefine`, logged as `"recalled memory injected into the worker prompt"`.

## 5. Interfaces

```go
// internal/memory
type Provenance struct{ ChatID, NodeID, Source string }

func (s *Store) Commit(ctx context.Context, sc Scope, author string,
    staged []Candidate, sourceText string, prov Provenance) (int, error)

// New: outcome events invalidate/reinforce by provenance, not by id.
type OutcomeKind string
const (
    OutcomeReinforced  OutcomeKind = "reinforced"
    OutcomeInvalidated OutcomeKind = "invalidated"
)
type OutcomeSignal struct {
    Kind   OutcomeKind
    Reason string // required when Kind == OutcomeInvalidated
}
func (s *Store) ApplyOutcome(ctx context.Context, chatID string,
    o OutcomeSignal) (int, error)
```

`ApplyOutcome` is backend-agnostic at the `Store` level; each `index` implementation gets a `updateStatus(ctx, chatID string, o OutcomeSignal) (int, error)` — a payload-only mutation (qdrant's `SetPayload`, sqlite's `UPDATE`), not a re-embed.

**Who calls it: nobody in the extension layer.** (Revised 2026-08-12, Jason's veto: memory must not leak into extensions.) The extension already reports the domain fact core needs — `Host.UpdateChatOrigin` fires on merged/closed/reopened. Memory lifecycle is core's INTERPRETATION of that fact: quack's `UpdateChatOrigin` implementation (internal/serve) observes the subject-state transition and calls `ApplyOutcome` itself, mapping chatID → minted memories via provenance. Extensions never learn memory exists, exactly as they never learn the trust gate exists.

One typed-string consequence: core must not branch on `Badge` (a display string — the SDK's ownership rule). `ChatOrigin` gains a small typed field the extension sets alongside the badge it already chooses:

```go
// sdk (quack-extensions/sdk) — additive
type SubjectState string // "" (unknown) | open | merged | closed
// ChatOrigin gains: State SubjectState
```

Core maps: transition→merged ⇒ `ApplyOutcome(chat, reinforced)`; transition→closed (not merged) ⇒ `ApplyOutcome(chat, invalidated, "subject closed unmerged")` — against every configured memory store (task + user — the same set `memStores()` in rest/memory.go iterates). Force-push detection drops out of v1 entirely (it was the only signal origin doesn't carry; #849's incident is fully covered by closed-unmerged). If it ever earns its way in, it arrives the same way: a typed domain fact on the origin update, never a memory API.

**The consolidator job**: new `internal/memory/consolidate_job.go`, started next to `ledger.RunRetentionSweep`'s call site in server bootstrap. `Store.RunConsolidationSweep(ctx, schedule, retentionDays)` — a standard 5-field cron loop (`""` disables); it waits for the first `Next`, never running at boot (issue #961).

**REST/OpenAPI**: `Memory` schema gains `status`, `reinforcement_count`, `invalidation_reason` (openapi.yaml + `make generate`). `DeleteMemory` gains an optional `reason` in the request body.

## 6. Forbidden

- Training or fine-tuning any model. The consolidation model is prompted, never trained — same model, same JSON-ops contract, new event vocabulary in its input.
- An LLM judge as the outcome gate. A judge score is prose plausibility, never truth — that's the #849 root cause repeating itself if we let it back in. The only gates in this design are subject merged/closed and human-delete: all deterministic, all already machine-visible.
- Memory concepts in the SDK. Extensions report typed domain facts (origin/SubjectState); core alone interprets them into memory lifecycle. No ReportMemoryOutcome-style callback, ever.
- Silent deletion. Every status transition writes a `memory_ops` row. Consolidator DELETE and outcome-feedback invalidation are the same `invalidated` status as a human delete, never a bare `remove`.
- Consolidation blocking a run. Both the per-commit reconcile and the periodic sweep stay fire-and-forget/background, same shape as `commitMemoryOnPass`'s goroutine.
- Unbounded `memory_ops` (or invalidated-point) growth with no stated bound. A retention sweep, same pattern as `ledger.RunRetentionSweep`, hard-deletes invalidated rows past a configured `retention_days` (0 = forever, explicit not implicit).

## 7. Test cases

1. **#849 replay.** `ci_fix` run mints 4 memories (`status=unverified`, `chat_id=X`) into `role:coding`. The reviewer PR that used them closes unmerged with a correcting comment. The extension's existing closed-unmerged branch updates origin with `State=closed`; core's UpdateChatOrigin observes the transition → `ApplyOutcome` invalidates all 4 (4 `memory_ops` rows, `actor=outcome-feedback`) → the next reviewer run's recall excludes them even though the points still exist in the index.
2. **Reinforcement promotion.** A repo-convention memory minted by chat `Y` survives to a merged PR. `ApplyOutcome(Y, reinforced)` bumps `reinforcement_count` 0→1, `status` unverified→reinforced. Recall now prefixes it `[reinforced ×1]` instead of `[unverified, single run]`.
3. **Force-push invalidation.** A memory minted while implementing on branch `B` at SHA `s1`. The user force-pushes `B` to `s2`, discarding `s1`'s commits. The new `synchronize` handler detects `s1` isn't an ancestor of `s2` → invalidate, reason `"head rewritten after this run's commits"`.
4. **Burst dedupe.** The consolidation sweep finds 3 unverified memories from one `chat_id` within a 5-minute window making near-identical claims. `decide()`, prompted with the cluster, emits 1 `ADD` + 2 `DELETE`(→invalidate, reason `"duplicate of <id>"`).
5. **Human delete audit trail.** A maintainer deletes a memory from the `/memory` tab. `status=invalidated`, `actor=human`, reason from the request (default `"manual delete"`), one `memory_ops` row. A default `GET /api/v1/memories` excludes it; an explicit `include_invalidated=true` still shows it with its reason.

## 8. Phasing

1. **Provenance stamping.** `Commit` gains `Provenance`; every call site (vetting/node.go, commit_memory.go) updates; new payload/columns are additive. Prerequisite for everything else. ~1 PR.
2. **Lifecycle fields + `memory_ops` + `ApplyOutcome` + recall status filter + tier prefix at the injection point.** ~1–2 PRs.
3. **`DeleteMemory` → invalidate.** REST + openapi.yaml + `make generate`. ~1 PR.
4. **`ChatOrigin.State` (typed SubjectState) + core-side outcome mapping.** Small sdk additive bump (the extension already computes merged/closed for the badge — one more field on the same update); quack's UpdateChatOrigin implementation observes the transition and calls ApplyOutcome. Memory never appears in the SDK.
5. **Periodic consolidator job** (burst-dedupe) + config `schedule` + retention sweep. ~1–2 PRs.
6. **Frontend**: `MemoryEntry`/`MemoryTab` render the tier badge and invalidation reason. ~1 PR, can land any time after step 2.

## 9. Future work

- **AttriMem-style attribution** (arXiv 2607.21106): which specific recalled memory actually influenced a given output, so reinforcement and invalidation can target real contribution instead of mere co-occurrence in the prompt.
- **Supersede-style update-gap eval** (arXiv 2606.27472): a standing benchmark measuring how long a contradicted memory survives before this pipeline corrects it — turns "did #849 happen again" into a number.
- **Temporal decay** (the other half of SSGM's checklist, deliberately deferred here): a memory nobody's outcome-feedback or consolidation cluster ever touches sits at `unverified` forever. A time-based decay-to-invalidated after N cycles with no reinforcement is future work, not designed in this pass.
