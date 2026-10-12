# Measuring the judge

The trust gate is only as good as its rubrics and its judge. This page covers the checks that tell you whether a rubric is well formed and whether the judge scores it consistently and correctly.

## Judge consistency

`quack judge replay <chat> --repeat N` judges each recorded round N times over the same rebuilt answer, so any difference between passes is the judge's own noise. Per judge-scored criterion it reports mean, sd, min, max, pass rate and agreement, where agreement is the fraction of repeats that agree with the majority pass/fail: 1 means the judge never flipped, 0.5 is a coin toss. The round gets the same for its overall pass/fail, and the report ends with a summary:

```text
consistency: <rounds> round(s), <cells> criterion cell(s), <unstable> unstable; criterion agreement <pct>, round agreement <pct>, max sd <sd>
```

`--json` prints one object, `{repeat, consistency, rounds}`, with floats rounded to four places and no timestamps; `--out <file>` writes the same bytes to a file. Store a baseline before a rubric or judge-prompt change and diff the next run against it:

```sh
quack judge replay <chat> --from-server <url> --repeat 5 --json --out before.json
# edit the rubric, then
quack judge replay <chat> --from-server <url> --repeat 5 --rubric agents/web-researcher/rubric.yaml --json --out after.json
diff <(jq .consistency before.json) <(jq .consistency after.json)
```

`unstable` counts the (round, criterion) cells whose repeats disagreed. A criterion that is unstable across many rounds needs a narrower definition of what one finding is (see the measured example in [Agents](agents.md)); one that always scores the same band on every answer is not measuring anything.

## Rubric lint

Every `rubric.yaml` under `agents/`, `extensions/*/plugin/agents/` and `config/` is linted by `TestShippedRubricsLintClean` in `internal/vetting`, which the `go-test` CI job runs whenever a PR touches those paths. `quack server validate` lints the rubric of every configured agent, plugin-seeded ones included, and fails on any issue. The lint runs boot's own validation (scale, band coverage, pass mark inside a band, a `fix` on every deterministic criterion) and adds:

- No unknown keys. A rubric has no weights: the round score is the weakest criterion, so a `weight:` key would be silently ignored and is rejected instead.
- Every criterion has a non-empty `definition`, and no two criteria share one.
- Every judged criterion has `bands`, so the judge has a pass bar to score against.
- A `deterministic: true` criterion names a check code actually runs (`cites_sources`, `specifics_cited`, `specifics_supported`, `checks_pass`, `sufficient_length` and the rest of `deterministicCriterionSpec`).
- A judged criterion does not reuse one of those names; code overwrites the judge's score for it.

Duplicate criterion names are a YAML parse error.
