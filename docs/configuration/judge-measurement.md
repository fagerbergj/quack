# Measuring the judge

The trust gate is only as good as its rubrics and its judge. This page covers the checks that tell you whether a rubric is well formed and whether the judge scores it consistently and correctly.

## Rubric lint

Every `rubric.yaml` under `agents/`, `extensions/*/plugin/agents/` and `config/` is linted by `TestShippedRubricsLintClean` in `internal/vetting`, which the `go-test` CI job runs whenever a PR touches those paths. `quack server validate` lints the rubric of every configured agent, plugin-seeded ones included, and fails on any issue. The lint runs boot's own validation (scale, band coverage, pass mark inside a band, a `fix` on every deterministic criterion) and adds:

- No unknown keys. A rubric has no weights: the round score is the weakest criterion, so a `weight:` key would be silently ignored and is rejected instead.
- Every criterion has a non-empty `definition`, and no two criteria share one.
- Every judged criterion has `bands`, so the judge has a pass bar to score against.
- A `deterministic: true` criterion names a check code actually runs (`cites_sources`, `specifics_cited`, `specifics_supported`, `checks_pass`, `sufficient_length` and the rest of `deterministicCriterionSpec`).
- A judged criterion does not reuse one of those names; code overwrites the judge's score for it.

Duplicate criterion names are a YAML parse error.
