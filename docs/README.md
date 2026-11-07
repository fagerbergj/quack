# Quack docs

The operator CLI is described in [cli.md](cli.md) and the QA rig in [qa.md](qa.md).

## Evidence for pull requests

A pull request into the trunk shows green CI (typecheck, tests, sloplint) and, for any change on the model path, a 1-3 item smoke test against local llm-swap, with the commands and output in the PR.

### Evidence for a judge or rubric change

Replay the same recorded cases under the baseline and the candidate, three times each, on local models, the same day:

```
QUACK_LOCAL_API_KEY=... QUACK_CLEF_API_KEY=... node packages/eval/src/replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --classifier clef/clef --arm base=<trunk ref> --arm cand=. --repeat 3 --concurrency 2
```

`cases.json` is an array of `{ case, kind, checks?, session?, version?, content?, evidence? }`. `kind` picks the extension's eval adapter (its rubric and evidence builder). `session` is a recorded session directory whose last (or `version`th) artifact of that kind is judged, with evidence rebuilt by the adapter from the recording as it stood when that version was sealed, through the same builder the live judge uses: `research.report` (fetch log, kept source files, request), `contract.spec` (artifacts, approvals, answered questions, workspace files) and every Sleeper card (the facts step result of the workflow run that sealed it, with the news checks dropped when the recorded verdict says the job had no news source, as the live judge drops them). `content` and `evidence` (scope to units) give both inline for a kind whose adapter cannot rebuild evidence, such as `github.review`. `checks` (`id` or `criterion.id`) limits the run to the checks being watched. An arm is `label=ref[:classifier]`: `.` is this tree, any other value is a git ref whose rubrics and evidence code are used while the judge engine stays this tree's, so a change to `packages/judge` itself is not visible to a baseline arm. The classifier is `none` or `provider/model`; without one the arm takes `--classifier`, or none. Without `--arm` it runs one arm, `current=.`.

To measure a classifier change, run two arms on the same tree:

```
... replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --arm base=.:none --arm clef=.:clef/clef --repeat 3
```

It prints a markdown table of per-repeat `met` (null: no unit, the check does not count) and classifier/escalation counts per case and check for each arm, then each arm's median and p90 judge seconds per case, then every case and check whose `met` values differ between arms with each arm's values and the judge's notes, for adjudicating false positives, and last any case an adapter declined with what its recording lacks. Paste it into the PR with the cases' provenance.

## Checking a rollout

Before `compose up` with a new image, run its validator against the prod config, agents and data volume, mounted read-only. It loads every bundle and every session's pinned `agent-def/agent.json` with the deployment-level extensions (unset extension env gets placeholders, nothing is contacted), prints `FAIL <path>: <error>` per failure and `N loaded, M failed`, and exits non-zero on any failure:

```
docker run --rm -v <data>:/data:ro -v <config>:/config/deployment.json:ro -e QUACK_CONFIG=/config/deployment.json -e QUACK_AGENTS_DIR=/app/agents -e QUACK_DATA_DIR=/data <new-image> node packages/runtime/src/validate.ts
```
