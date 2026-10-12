# Extensions de-slop plan (re-scoped 2026-09-15)

Scope: the ENTIRE repo (all five modules), same protocol as
`../quack/.quack/deslop-plan.md`. Supersedes the per-phase path-ownership
split below - kept only as a map of where the code lives.

Repo-specific rules (unchanged):
- **The invariant is load-bearing:** nothing here may import `quack`;
  extensions import only the SDK.
- **SDK changes are contract changes.** De-slop of `sdk/` is add-or-document
  only; public-API findings go to `.quack/deslop/deferred-sdk-findings.md`
  for a coordinated version bump.
- Tests are per-module: `go test ./...` inside each module dir (no Makefile
  here); `.github/workflows/ci.yaml` is the authoritative gate.
- Exclude from counts: `go.sum`, `cmd/qa-mock` fixtures, `*_test.go`
  fixtures and table bodies.

## Feature map

### L0-1: Extension contract
- **SDK** — `sdk/` (980 LOC incl. tests) - contract types, Host, Factory

### L0-2: First-party extensions
- **GitHub** — `github/` (~7.3k LOC) - webhooks, CI auto-heal, permissions,
  artifact delivery, `cmd/qa-mock`
- **Remarkable** — `remarkable/` (~1.1k) - tablet document browser -> ingest
- **Usage** — `usage/` (~360) - Prometheus usage dashboard (inbound-only)
- **Noop** — `noop/` (~120) - proves the dispatch->observe loop

## Waves

- **Wave 1 - the gates (this first PR):** `.golangci.yml` at the repo root
  (dupl 60 + the defect tier, shared by all five module jobs - golangci
  finds it by walking up from each module dir), `tools/sloplint/` (port of
  quack's, plus an optional root arg for the not-a-module-at-root layout),
  and the `go-slop` CI job (changed-code-only: `--new-from-rev` per module
  + one repo-wide `sloplint diff`). Baseline: `gates-baseline.md`.
- **Wave 2 - CC backlog:** the 9 over-15 functions in `github/`
  (merge.tryMerge 35, webhook.dispatch 29, snapshot.diffSnapshots 29,
  webhook.handlePullRequest 28, envelope.deliverableText 24,
  extension.applyDefaults 23, run.finalize 21, tools.deliverOne 20,
  internal/httpx.RoundTrip 16). Webhook/hMAC branchiness gets the
  inherent-shape check first. One PR.
  **DONE - PR #88 merged 5334e30 (2026-09-15).** Seven split to <=15;
  handlePullRequest (20) + RoundTrip (16) kept as inherent (handlePullRequest
  carries the new `sloplint: cc-allow <reason>` exemption). The review
  caught a real defect the split introduced (mergeNoIntent == 0 read as
  "no blocker" - closed PRs reached mergePR) - fixed with a blocked bool
  + regression test. Also in #88: cc-allow exemption in tools/sloplint
  (mirrors quack #1412, incl. the window off-by-one fix + boundary tests),
  the invalid formatters.default key dropped, and the tool's own tests
  wired into the go-slop job.
- **Wave 3 - dup backlog:** webhook.go:270-285 vs 288-303 (the one
  non-test dup pair; the 13 dupl hits in delivery_test.go are test
  scaffolding - extract only if a shared helper is genuinely cleaner).
  Fold into Wave 2 if it stays small.
  **DONE - folded into Wave 2 (#88) as the labeledTrigger helper.**
  Remaining dup is test scaffolding only (delivery_test.go).
- **Wave 4 - pattern sweep:** the un-numbered patterns (trivial wrappers,
  dead defensive code, once-used vars, narrative comments) across all five
  modules, biggest first.

Frozen across all waves: every `go.mod`/`go.sum` (unless a dependency
bump is the point of a PR), `README.md` (SDK changelog is source-of-truth
prose), `.github/` (except the gate job itself).

## Done when
- Gates green on main; `gates-baseline.md` superseded by
  `<wave>-metrics.md` before/after numbers.
- `go test ./...` green in all five modules; CI green.
- Zero public-API changes; recorded SDK findings in
  `.quack/deslop/deferred-sdk-findings.md`.
