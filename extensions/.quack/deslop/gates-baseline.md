# Wave 1 baseline — pre-gate measurements (2026-09-15)

Measured with the ported `tools/sloplint` (CC + 6-line statement runs) and
golangci-lint dupl (60 tokens). The gates land changed-code-only; this is the
backlog they will measure going forward.

## CC > 15 (9, all in github/)

| CC | Function | File |
|---|---|---|
| 35 | tryMerge | github/merge.go |
| 29 | dispatch | github/webhook.go |
| 29 | diffSnapshots | github/snapshot.go |
| 28 | handlePullRequest | github/webhook.go |
| 24 | deliverableText | github/envelope.go |
| 23 | applyDefaults | github/extension.go |
| 21 | finalize | github/run.go |
| 20 | deliverOne | github/tools.go |
| 16 | RoundTrip | github/internal/httpx/transport.go |

CC 10-15 (not gated; context): snapshot.fetchSnapshot 15, cifix.autoHeal 12.
sdk/, remarkable/, usage/, noop/: zero functions over 15.

## dupl (60 tokens, non-test)

- github/webhook.go: 270-285 and 288-303 (the labeled-trigger invoker
  blocks - the merge and ci_fix label handlers share their
  allow-check + log + kick-off shape).
- 13 dupl hits in delivery_test.go (test scaffolding - excluded from the
  backlog per the plan; extract only if a helper is genuinely cleaner).

## 6-line statement runs (sloplint repo mode)

Reported alongside CC in the wave metrics; the gate itself does not
enforce run counts (only CC and comment runs on changed code).

## sdk/ (Phase 1 of the old plan)

Clean - see `phase1-metrics.md`. No PR; no public-API findings.
