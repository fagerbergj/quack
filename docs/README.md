# Quack docs

The operator CLI is described in [cli.md](cli.md) and the QA rig in [qa.md](qa.md).

## Evidence for pull requests

A pull request into the trunk shows green CI (typecheck, tests, sloplint) and, for any change on the model path, a 1-3 item smoke test against local llm-swap, with the commands and output in the PR.

### Evidence for a judge or rubric change

Replay the same recorded cases under the baseline and the candidate, three times each, on local models, the same day:

```
QUACK_LOCAL_API_KEY=... QUACK_CLEF_API_KEY=... node packages/eval/src/replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --classifier clef/clef --arm base=<trunk ref> --arm cand=. --repeat 3 --concurrency 2
```

`cases.json` is an array of `{ case, kind, checks?, units?, session?, version?, content?, evidence?, inputs? }`. `kind` picks the extension's eval adapter: its rubric with its declaration and inputs builder, or, for a rubric not yet declared, its evidence builder. `session` is a recorded session directory whose last (or `version`th) artifact of that kind is judged, with evidence rebuilt by the adapter from the recording as it stood when that version was sealed, through the same builder the live judge uses: `research.report` (fetch log, kept source files, request), `contract.spec` (artifacts, approvals, answered questions, workspace files) and every Sleeper card (the facts step result of the workflow run that sealed it, with the news checks dropped when the recorded verdict says the job had no news source, as the live judge drops them). `content` and `evidence` (scope to units), or `inputs` (name to text) for a declared rubric, give them inline for a kind whose adapter cannot rebuild them, such as `github.review`. `checks` (`id` or `criterion.id`) limits the run to the checks being watched, and `units` (ids such as `finding-14`) to those units of an undeclared rubric's evidence, or to those items of the watched checks' scopes of a declared one (an item's id, or its unit's `claim finding-14`), so one finding of a long report is a case of its own. An arm is `label=ref[:classifier]`: `.` is this tree, any other value is a git ref whose rubrics and evidence code are used while the judge engine stays this tree's, so a change to `packages/judge` itself is not visible to a baseline arm. The classifier is `none` or `provider/model`; without one the arm takes `--classifier`, or none. Without `--arm` it runs one arm, `current=.`. `--escalations` bounds each case's concurrent judge-model calls (4 by default, as in the live judge), so the judge model sees at most `--concurrency` times that at once.

To measure a classifier change, run two arms on the same tree:

```
... replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --arm base=.:none --arm clef=.:clef/clef --repeat 3
```

It prints a markdown table of per-repeat `met` (a decided fold as 1 or 0; null: no unit or an undecided fold, the check does not count) and classifier/escalation counts per case and check for each arm, then each arm's median and p90 judge seconds per case, then every case and check whose `met` values differ between arms with each arm's values and the judge's notes, for adjudicating false positives, and last any case an adapter declined with what its recording lacks. Paste it into the PR with the cases' provenance.

## Checking a rollout

Before `compose up` with a new image, run its validator against the prod config, agents and data volume, mounted read-only. It loads every bundle and every session's pinned `agent-def/agent.json` with the deployment-level extensions (unset extension env gets placeholders, nothing is contacted), prints `FAIL <path>: <error>` per failure and `N loaded, M failed`, and exits non-zero on any failure:

```
docker run --rm -v <data>:/data:ro -v <config>:/config/deployment.json:ro -e QUACK_CONFIG=/config/deployment.json -e QUACK_AGENTS_DIR=/app/agents -e QUACK_DATA_DIR=/data <new-image> node packages/runtime/src/validate.ts
```
