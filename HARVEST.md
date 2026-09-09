# Review-suggestion harvest triage (worktree-only, not committed)

Verified against origin/main at be57f1e. "fixed here" = in this branch's working tree; "other fork" = implemented by the sibling agent in this worktree (reviewed, correct).

| Item | Verdict | Where / why |
|---|---|---|
| Dead warm-replay branch, rest/handler.go SubscribeChatStream | fixed here (other fork) | `done` implies empty replay + !active (Hub.Close nils buf), so the cold path already returns; branch removed, comment explains |
| Stale sendID comment, rest/sse.go | fixed here (other fork) | sendID is the only writer and serves both POST stream and subscribe |
| Stale store.go:41 index.list doc | already fixed | no such doc in internal/store/store.go; the index.list docs live in internal/memory/store.go and match the code |
| Page-token sort binding | fixed here (other fork) | token encoded a constant "recency_desc", never the requested sort; now bound to sort + bucket, openapi + generated updated |
| groupByAge memo | already fixed | MemoryTimeline.tsx useMemo, pinned by MemoryTimeline.perf.test.tsx (#1286) |
| Suspense fallback a11y / prefetch | fixed here (a11y) / rejected (prefetch) | App.tsx: role=status "Loading…" fallback instead of null. Route prefetch is a new feature, not an unaddressed suggestion; out of scope |
| SynthesizeChatEvents stamps time.Now() | fixed here (other fork) | fold.NodeState carries StartedAt/TerminalAt from the entry's At; runlog/fold.go builds events from those |
| No focus trap in NodePopup/ContentPopup | already fixed | both go through Sheet -> useDrawer (Esc, Tab trap, focus return); NodeMemoriesPanel/AttachmentUI use native dialog.showModal. No ContentPopup component exists |
| ChatMenu + ChatList kebab lose focus on Escape | already fixed | ChatMenu via Sheet/useDrawer opener refocus; ChatRow.close() refocuses btnRef (#1319) |
| NodePopup input 12px triggers iOS zoom | fixed here | three input/textarea text-xs -> text-base (Composer already uses text-base for the same reason) |
| max-h uses vh not dvh | fixed here | Sheet.tsx 90dvh (the shared mobile sheet), NodeMemoriesPanel 80dvh, AttachmentUI 92dvh |
| ledger recover logs "chat row is gone; skipping chat=unscoped" | already fixed | no non-test code produces a literal "unscoped" chat id: ledger.Exporter drops entries with an empty conversation id (#617), so ls.List never yields it |
| check-no-emoji.mjs line-numbered allowlist | fixed here | same-line `// allow-emoji: <why>` pragma replaces file:line entries; all five old entries were already stale (Composer lines no longer carry emoji; NavRail prose reworded) |
| .gitignore negation not honoured by buildDirGrants | fixed here (other fork) | `!name` deletes an earlier bare/anchored match; also .git bonus-grant and tracked-regular-file guards from the #1321 review, with tests |
| missingMemoryVotes checks "any vote" not "every id" | fixed here (other fork) | per-id coverage; node.go's duplicated inline check now calls the shared helper |
| ReadEntriesFilteredSince full scan | fixed here | pgstore.go: CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ledger_kind_at (kind, at), same boot path as the idempotency index |
| Cancel-after-delivery race (dagStream vs node.go) | fixed here | NodeControl.MarkDelivered() (vetting/node.go, called at commitDelivery) -> nodeControl.owner sticky runControls.delivered map (dag/control.go, survives unregister like cancelled/paused) -> dagStream.deliveredOf, checked first in handle()'s switch and in DagStream.Finish() - outranks a live pause/cancel that raced in after delivery, not just the out!="" guess. Test: TestDagStream_DeliveredOutranksLivePauseRace |
| TestRunPlanAsGraph_ResumeDoesNotResetReviewFanout under -count>1 | already fixed | test has t.Cleanup(vetting.ResetReviewFanout); -race -count=3 green (host cap forbids -count=50) |
| DagNode stories started_at_ms: 0 huge elapsed | rejected: not reproducible | Chat.stories finishedDagTurn uses started 0 / finished 12_000 and 12_000 / 20_000; LiveTimer renders finishedAt-startedAt (12s, 20s total). startedAt: 0 is the deliberate fixture convention across DagNode.stories |
| REST handler owns a separate EventLog | rejected: intentional | serve.go:636 documents why: a boot-resume backlog must not delay a live REST run's drain |
| 306 unvoted memory recalls / ACP vote drop | partially addressed | no ACP-specific vote-drop path found; the partial-vote bug in missingMemoryVotes (shared code, every agent) let 1-of-N verdicts pass silently, which is the plausible accumulator |
| Add check-icons to CI | fixed here | ci.yaml frontend-build: check-no-emoji and check-icons steps (neither ran in CI; both are in `npm run lint`) |

## gh api sweep (quack #1300-#1343 inline review comments tagged suggestion/nit/todo/question)

| Comment | Verdict |
|---|---|
| hub.go EndRun blind runs.Delete (#1342 review, x2) | fixed here (other fork): compare-and-delete by responseID, FinishRun threads it |
| runlog.go stale "flushes then closes then unregisters" at five call sites | fixed here (other fork) |
| sandbox.go .git bonus grant; regular-file escape (#1321 review) | fixed here (other fork) |
| agentStream.ts:355 node_done finishedAtMs unguarded | fixed here |
| agentStream.ts:521 "(used by the job live log)" stale | fixed here |
| store.go:1620 "doubled (x8)" | fixed here |
| ledger/store.go:85 MemStore fallback doc stale | fixed here |
| check-no-emoji stale Chat.tsx/Composer entries (x3) | fixed here (pragma replaces the list) |
| handler.go streamHub `!ok` comment under-describes drop/reset | fixed here (other fork) |
| answerdedup.go:52 empty-needle Contains; node.go:845 dedupe on every exit path | not done: behaviour change in the vetting gate, wants its own PR |
| store.go:1715 GetResponse double load; :1643 multi-plan pick; :1651 tail loader strictness | not done: design questions for the owner |
| stream/event.go:464 rebuilt duration ~0ms | superseded by the fold timestamp fix above |
| remaining nits (test naming, test-coverage suggestions, comment wording in acp.go/openai.go/format.go/gitprobe.go/reviewrecord.go, ChatList aria-label collision, arrow-key tests) | not done: non-blocking, each merged with the comment open; listed for the comment-audit lane |

Not swept: fagerbergj/quack-extensions #70-#79 (out of this agent's budget; comment-audit lane).
