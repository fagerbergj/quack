---
name: go-logging
description: |
  Standards and gotchas for structured logging in the quack Go backend with log/slog. Covers the
  log-or-return rule (log once at the handling boundary, never log-and-return), the Debug/Info/Warn/Error
  decision matrix (reserve Error for the exceptional), what/when to log by layer, native attrs over
  fmt.Sprintf, secret/PII redaction, and quack's own conventions: one slog default handler set in main
  (QUACK_LOG_LEVEL/QUACK_LOG_FORMAT env, text default, stdout), a `component` attribute per subsystem, a per-instance
  `*slog.Logger` field for objects with stable identity (e.g. the vetting gate's `g.log`), hot-path
  per-round trace at Debug, and bridging third-party loggers (GORM) through slog.
  Use when adding, reviewing, or converting logging in any Go file under internal/ or cmd/ — choosing a
  level, deciding whether to log an error, naming attributes, or wiring a logger.
  Do NOT use for frontend logging, OpenTelemetry/tracing setup, or non-Go code.
license: MIT
metadata:
  author: jason
  version: "1.0"
---

# Go Logging (log/slog) — Quack Backend

## Overview

How to log in quack's Go backend: which level, whether to log at all, what attributes to attach,
and how loggers are wired. The backend uses the standard library `log/slog` exclusively — no third-party
logger. This skill governs the *decisions* (level, log-vs-return, attribute naming); concrete code
patterns live in `references/recipes.md`.

## When to Use

- Adding a log line to any file under `internal/` or `cmd/`.
- Reviewing or converting a diff that touches logging (`log.Printf` → slog, picking levels).
- Deciding whether an error should be logged here or returned to the caller.
- Naming log attributes or wiring a logger into a new subsystem/struct.

## When NOT to Use

- Frontend logging (`frontend/`) — different stack.
- OpenTelemetry / distributed tracing setup — out of scope (see the M12 observability milestone).
- Non-Go code.

## The two rules that matter most

**1. Log-or-return — never both.** An error is either *handled* or *propagated*, not both:

- **Low layers** (services, repos, tool/model clients): `return err`, wrapping at package boundaries
  with `fmt.Errorf("query users: %w", err)`. **Do not log.** One short context phrase per wrap — no
  stacked "failed to … failed to …".
- **Handling boundary** (HTTP handler, `main`, or a point that *recovers* — swallows the error and
  continues with a fallback): log it **once**, with the error as an `"err"` attribute.

  Logging at every layer turns one failure into a wall of duplicate lines that hide the root cause.

**2. Reserve Error for the exceptional.** Most failures that the code recovers from are `Warn`, not
`Error`. Use the matrix:

| Level | Use for | Quack examples |
|---|---|---|
| **Debug** | Routine/internal trace, noisy in prod, useful when diagnosing | per-round gate trace (worker start, each self-critique/revise round, retrieval inventory), compaction prune/reuse/summarise decisions, executor "built task" |
| **Info** | Significant operational events an operator wants to see normally | startup ("quack listening"), agent serving, "node done", judge verdict, "vetted answer ready", plan executed |
| **Warn** | Expected, recoverable anomaly / fallback path taken — core path survives | compaction failed→continuing uncompacted, title gen failed→empty title, tool-call cap hit, empty-answer fallback, no-progress stop, best-effort persist failed |
| **Error** | Exceptional — primary op failed, external service down, needs attention | worker stream error, judge (external model) unavailable, startup DB op failed |

Litmus for Error vs Warn: *did the operation's primary purpose fail in a way an operator should look
at?* If it's an optimization, a cosmetic feature, or a graceful fallback, it's `Warn`.

## What & when to log

- Log **what happened** (domain outcome + IDs), not how it was done. Attach IDs that let a single
  event be re-investigated: `node`, `plan`, `session`, `agent`.
- One information-dense line per significant operation (completion summary) beats many fragments.
- **Do not** log every iteration of a tight loop at Info, giant payloads, nil/empty values, or every
  DB query. Slow queries only, at Warn (the GORM bridge already does this).
- Hot paths (the vetting gate's per-round loop, compaction per-call) log at **Debug** so default Info
  is quiet; `QUACK_LOG_LEVEL=debug` brings the firehose back.

## Quack conventions (match these)

- **One default handler**, set once at the top of `main()` via `setupLogging()` — never construct
  loggers ad hoc elsewhere. Config is env: `QUACK_LOG_LEVEL` (debug|info|warn|error, default info),
  `QUACK_LOG_FORMAT` (text default, `json` for aggregators). Output goes to **stdout**.
- **`component` attribute** identifies the subsystem (`vetting`, `dag`, `agent`, `execute`, `title`,
  `store`, `startup`) — it replaced the old `subsystem:` string prefix.
- **Package-level helpers / one-off sites**: call `slog.Info(msg, "component", "dag", …)` directly.
  Do **not** create a package-level `var logger = slog.With(...)` — see Gotchas.
- **Objects with stable identity** (the vetting gate, built per worker): hold a pre-tagged
  `log *slog.Logger` field set **in the constructor** — e.g. `g.log = slog.With("component","vetting","agent",name)`.
  This DRYs the repeated identity attrs and is init-order-safe (constructor runs after `setupLogging`).
- **Fatal**: slog has none. In `main` only, use the `fatal(msg, args...)` helper (`slog.Error` + `os.Exit(1)`).
  Never `os.Exit`/`log.Fatal` below `main` — it skips deferred cleanup; return the error instead.
- **Third-party loggers** (GORM): bridge through slog with
  `slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn)` so their output shares the handler.

## Gotchas

- **Package-level `slog.With` captures the wrong handler.** A `var logger = slog.With(...)` runs at
  import time, *before* `main` calls `slog.SetDefault`, so it binds the throwaway default handler.
  Use inline `slog.Info(..., "component", x)` or a struct field set in a runtime constructor.
- **Parse `QUACK_LOG_LEVEL` with `slog.Level.UnmarshalText`, not a hand-rolled switch.** It's case-insensitive
  and leaves the zero value `LevelInfo` on empty/garbage input — exactly the wanted default, in two lines.
- **`go vet` is the correctness gate for slog.** It catches odd/mismatched key-value pairs across all
  call sites. A clean `make vet` is required; treat a vet finding as a real bug.
- **Never `fmt.Sprintf` inside a log call.** `slog.Info("x", "k", v)`, not `slog.Info(fmt.Sprintf(...))`.
- **`time.Duration` formats natively** — pass it as an attr (`"dur", time.Since(t0)`); drop the `.Round()`.
- **`LogValue()` is bypassed on struct fields reached by reflection.** Logging a whole struct that
  contains a secret field leaks it in plaintext — see `references/recipes.md` for redaction.

## Validation Loop

After any logging change:

1. `go build ./...` — compiles.
2. `make vet` — the slog analyzer passes (no mismatched attrs). **This is the gate.**
3. `gofmt -l internal cmd` — empty.
4. `go test ./...` — green (conversions are behavior-preserving; existing tests guard them).
5. Eyeball levels against the matrix: any `Error` on a recovered/optional failure → downgrade to `Warn`.

## Resources

- Read `references/recipes.md` for concrete code: `setupLogging`, the `fatal` helper, per-instance
  logger fields, the GORM bridge, secret/PII redaction (`LogValuer` / `ReplaceAttr` / masq),
  request-scoped child loggers + `context.Context` correlation, and canonical log lines.
