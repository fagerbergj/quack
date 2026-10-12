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

## Golden set

Consistency says nothing about whether the judge is right. `testdata/judge-golden/` is a labeled set: each item is an answer, the rubric it is graded under, and a human pass/fail per criterion with the reason. `quack judge score-golden <dir>` grades every item through the live gate's own functions (`vetting.ReplayRound`, the same path `quack judge replay` uses) and compares each labeled criterion's pass/fail at the set's threshold with the label:

- A false pass is a criterion the label fails and the gate passes. Its rate is over the labels that say fail.
- A false fail is a criterion the label passes and the gate fails. Its rate is over the labels that say pass.
- A criterion the round did not produce cannot fail it (the round score is the weakest criterion present), so it counts as a pass, with `scored: false` in the report.

The run exits 2 when any rate, per criterion or overall, exceeds the limit in the set's `config.json`, and 1 when an item errored or nothing was scored:

```json
{"threshold": 0.6, "max_false_pass_rate": 0, "max_false_fail_rate": 0}
```

`threshold` is required and should match the deployment's `gates.judge.threshold`. The checked-in set's limits are zero because its scored items are deterministic; for a live run, copy the set and loosen them. `TestScoreGoldenSeedSetAgreesOffline` runs the set offline in CI.

An item is one JSON file. An inline item carries the task, the answer and the URLs the worker fetched:

```json
{
  "id": "cites-fabricated",
  "rubric": "../../agents/web-researcher/rubric.yaml",
  "task": "Which year did the Golden Gate Bridge open?",
  "answer": "The bridge opened in May 1937 ([history](https://example.com/golden-gate-history)), and ... ([review](https://bridges.example.org/golden-gate-review)).",
  "activity": {"fetched": ["https://example.com/golden-gate-history"]},
  "labels": {"cites_sources": {"pass": false, "reason": "bridges.example.org was never fetched or searched"}}
}
```

A recorded item points at one judged round of a recording instead of `task`/`answer`/`activity`, and is rebuilt exactly as `quack judge replay` rebuilds it: `"recording": {"bundle": "recordings/x.zip", "node": "node-1", "round": 1}`. Paths are relative to the item file. `require_retrieval: true` sets the research-agent policy that scores `grounded_in_retrieval`.

Labels on judge-scored criteria need `"needs_live_model": true`; `--deterministic-only` skips those items, and a live run sends only them to the judge. The loader rejects a judge-scored label on an item without the flag, and a label on a criterion that needs a live workspace (`checks_pass`, `artifact_valid` and the rest of `vetting.EnvironmentOnlyCriteria`).

The seed set covers `cites_sources` (backed and invented links, inline and recorded), `sufficient_length`, `no_tool_call_syntax`, `grounded_in_retrieval` and `specifics_cited` (cited and uncited figures), plus three live items on the web-researcher's `answers_question` and `internally_consistent`.

### Adding items from real recordings

1. Find a round worth labeling, typically one a human disagreed with: `quack ledger list`, then `quack ledger export <chat-id> -o testdata/judge-golden/recordings/<name>.zip`.
2. See the round's criteria and scores: `quack judge replay testdata/judge-golden/recordings/<name>.zip --deterministic-only --json` (drop `--deterministic-only` to include the judge's criteria).
3. Write `testdata/judge-golden/<name>.json` with `recording` pointing at the node and judge round, the agent's `rubric`, and a label with a reason for each criterion you have checked by hand. Set `needs_live_model` when any label is judge-scored.
4. Run `quack judge score-golden testdata/judge-golden --deterministic-only` and commit the bundle with the item. A recording carries the chat's full text; export only chats whose content may live in the repository.

## Judge round timing

A slow judge round (a trivial one-node lookup once took 2m11s) is split by phase in two places: the round's `judge_round` record, the artifact revision the round writes as its write-ahead-log entry, carries a `timing` object, and every phase is recorded in the `quack.judge.phase.duration` histogram (seconds) with `agent` and `phase` attributes:

| Phase | What it covers |
|-------|----------------|
| `prepare` | The activity scan, the round's episodic revision and resolving the judge prompt. |
| `deterministic` | `computeDeterministicCriteria`: citation backing, length, `checks_pass` (which runs the node's checks), artifact schema, `specifics_cited`. |
| `admission` | Waiting for a judge-model slot; absent when no admission controller is wired. |
| `verify` | The `specifics_supported` page check; its metric point carries `criterion=specifics_supported`. |
| `judge_tools` | Each judge session from its start to its last tool result: the model turns that read files or artifacts, and the reads themselves. |
| `judge_verdict` | Each judge session from its last tool result (or its start) to the verdict, including any nudge or forced close. |

The record also counts `judge_calls` (judge sessions: more than one means a transient retry, a no-verdict retry or a re-judge after a pass with no reads), `judge_turns` and `tool_calls`:

```text
"timing": {"phase_seconds": {"<phase>": <seconds, to the ms>, ...}, "judge_calls": <n>, "judge_turns": <n>, "tool_calls": <n>}
```

Read a slow round as: `judge_calls` above 1 points at the re-judge and retry paths, a high `tool_calls` with most time in `judge_tools` at the read loop (bounded by `gates.judge.max_iterations`, default 14 when unset), and time in `judge_verdict` alone at the model's final generation. The whole round's wall time is the `gate.judge` span's duration in traces.

## Rubric lint

Every `rubric.yaml` under `agents/`, `extensions/*/plugin/agents/` and `config/` is linted by `TestShippedRubricsLintClean` in `internal/vetting`, which the `go-test` CI job runs whenever a PR touches those paths. `quack server validate` lints the rubric of every configured agent, plugin-seeded ones included, and fails on any issue. The lint runs boot's own validation (scale, band coverage, pass mark inside a band, a `fix` on every deterministic criterion) and adds:

- No unknown keys. A rubric has no weights: the round score is the weakest criterion, so a `weight:` key would be silently ignored and is rejected instead.
- Every criterion has a non-empty `definition`, and no two criteria share one.
- Every judged criterion has `bands`, so the judge has a pass bar to score against.
- A `deterministic: true` criterion names a check code actually runs (`cites_sources`, `specifics_cited`, `specifics_supported`, `checks_pass`, `sufficient_length` and the rest of `deterministicCriterionSpec`).
- A judged criterion does not reuse one of those names; code overwrites the judge's score for it.

Duplicate criterion names are a YAML parse error.
