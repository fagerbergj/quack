---
type: "Reference"
title: "Trust Gate"
openwiki_generated: true
---

# Trust Gate

Every node output passes through a **trust gate** before the DAG propagates its result. The gate runs a **generate → critique → revise → judge** loop to guard against confident-but-wrong output.

## Gate Structure

`RunGatedRefine` (in `internal/vetting/node.go`) runs the worker, then loops through stages:

1. **Continuation** — Mechanical completion signals (empty answer, undelivered commit, unposted review) hand the worker another tool-bearing round, up to 4
2. **Deterministic checks** — Citation backing, length, delivery/review/behavior criteria, and repo `checks` (build/vet/test commands)
3. **Independent judge** — Separate model scores G-Eval style; weakest-link criterion, threshold default `0.7`

Returns `(answer, result, err)` where `result` carries final verdict (score/passed/feedback/rounds).

## Deterministic Checks

`checksPassCriterion` in `internal/vetting/checks.go` runs node `Checks` or derives them from the repo:

- **cfg.Checks** — Plan-time validated command prefixes (argv-safe, no shell)
- **cfg.DeriveChecks** — Set true for `code-implementer`; discovers from cloned repo
- **workspace.RunPipeline** — Runs commands argv-only (pipes are native, no shell interpretation)
- **Stops at first failure** — Failing check scores 0 (weakest-link), reason included in revise feedback
- **Capped output** — `maxCheckOutputChars = 2000` prevents unbounded compile/test output

### Delivery Check

The delivery criterion (in `internal/vetting/delivery.go`) ensures tasks that say "commit/push/open-a-PR" actually do:

- **ImplementationIntent** — Both `implVerbRe` (add/implement/create/fix/etc.) AND `deliveryRe` (pull-request/commit/push/branch) must match in prose (not identifiers/URLs)
- **Directed check** — "review PR #4 and fix bugs" IS directed (impl-directed + deliver-directed); "review branch add-foo" is NOT (no directive)
- **Implied chain** — Opening a PR demands push, which demands commit

### Deterministic Criteria

Each criterion produces a `criterionScore` (0–1 normalized):

| Criterion | Check | Failure impact |
|-----------|-------|----------------|
| `cites_sources` | Inline citations present | Scores 0 if missing |
| `length` | Min/max token counts | Scores 0 if too short/long |
| `checks_pass` | Repo build/vet/test commands pass | Scores 0 if any check fails |
| `behaviour` | No forbidden tool calls or harmful content | Scores 0 if violated |
| `reviewed` | Code reviewer posts structured verdict | Scores 0 if not posted |

## Independent Judge

The judge is an **independently-configured model** (different weights/provider from worker) for genuine different-weights independence.

### System Prompt Layers

`internal/vetting/judge.go` composes the judge's prompt:

1. **Identity** — "You did NOT write the answer being evaluated, and you must not trust its assertions"
2. **Capabilities** — Tool-less OR read-only workspace tools (read_file, list_dir, glob, grep) + skill tools (list_skills, load_skill, load_skill_resource)
3. **Behavior** — Score each criterion 0–10 (G-Eval style), reason before scoring, weakest-link overall
4. **Termination** — Call `submit_verdict` tool exactly once with `{criteria, score, feedback}`

### Judge Tools

- **read-only workspace tools** — `read_file`, `list_dir`, `glob`, `grep`
- **skill tools** — `list_skills`, `load_skill`, `load_skill_resource`
- **termination tool** — `submit_verdict({criteria, score, feedback})`

### Score Normalization

`normalizeScale` converts integer 0–10 to 0.0–1.0 for gate thresholds.

### Criteria

Each criterion is scored 0–10; overall score is the **weakest-link**:

- `grounded_in_retrieval` — Claims backed by cited sources
- `grounded_in_code` — Code matches repo conventions, edge cases handled, tests pass
- `task_completeness` — Task requirements fully satisfied
- `cites_sources` — Inline citations present for claims
- `behaviour` — No harmful or forbidden behavior
- `reviewed` — Code reviewed per repo standards

## Revision Loop

If the judge's score < `cfg.Threshold` and `round < cfg.JudgeRounds`:

- `composeFeedback` builds a self-contained prompt: original task, worker's answer, judge's reason + score per criterion
- Worker re-drafts with feedback; loop repeats
- Max `cfg.JudgeRounds` (default 1) before giving up

## Gate Result

`GateResult` summarizes the outcome:

```go
type GateResult struct {
    Passed   bool        // score >= threshold
    Score    float64     // lowest criterion (0.0–1.0)
    Feedback string      // actionable notes
    Rounds   int         // judge rounds run
}
```

Written to session state:
- `quack.gate_failed/{nodeID}` — true if failed
- `quack.gate_score/{nodeID}` — final score
- `quack.gate_passed/{nodeID}` — passed flag
- `quack.gate_rounds/{nodeID}` — rounds count

## Continue-But-Warn

On gate failure, downstream nodes **continue but warn**:

- `continue-but-warn` flag attached to node output
- Dependent nodes can decide whether to proceed
- Gate result (score/feedback) included in error message

## Config

Per-node gate configuration in `vetting.Config`:

| Field | Default | Purpose |
|-------|---------|---------|
| `Threshold` | 0.7 | Minimum score to pass |
| `JudgeRounds` | 1 | Max judge/revise iterations |
| `Checks` | []string | Deterministic check commands |
| `DeriveChecks` | false | Derive checks from repo |
| `NodeID` | - | Workspace scope |
| `Task` | - | For delivery check |
| `Agent` | - | For observability |

See `internal/config/config.go` for global gate defaults.
