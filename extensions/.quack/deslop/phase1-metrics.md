# Phase 1 metrics — sdk/ (2026-09-15)

Scope: `sdk/` (registry.go + sdk.go; tests excluded from counts).

## Numbers

| Measure | Before | After |
|---|---|---|
| Functions with CC > 15 | 0 | 0 |
| Highest CC (non-test) | 5 (`Registered`'s loop shape; sweep reports only >15) | — |
| dupl findings (golangci dupl, 60 tokens) | 0 | 0 |
| 6-line+ dup runs (intra-slice) | 0 | 0 |

## Pattern scan

No findings. The slice is a pure API contract: types, constants, and eight
optional interfaces, each with a live consumer in `github/`, `remarkable/`,
`usage/`, or `noop/`. `registry.go` is 33 lines of registration with a
deliberate init-time panic on duplicate names.

Things that LOOK cuttable and are not:
- The "factories must be side-effect free" wording appears in the package doc,
  `Starter`'s doc, and `Factory`'s doc — deliberate contract emphasis at the
  three places a reader lands, not narrative drift.
- `Host.EnsureContextDir` is `Deprecated:` — kept until its last consumer;
  removing it is a public-API break, which this phase cannot make.

## Cuts

None — no PR for this phase.
