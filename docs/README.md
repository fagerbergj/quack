# Quack docs

The operator CLI is described in [cli.md](cli.md) and the QA rig in [qa.md](qa.md).

## Evidence for pull requests

A pull request into the trunk shows green CI (typecheck, tests, sloplint) and, for any change on the model path, a 1-3 item smoke test against local llm-swap, with the commands and output in the PR.

### Evidence for a judge or rubric change

Replay the same recorded cases under the baseline and the candidate, three times each, on local models, the same day:

```
QUACK_LOCAL_API_KEY=... QUACK_CLEF_API_KEY=... node packages/eval/src/replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --decision-model clef/clef --arm base=<trunk ref> --arm cand=. --repeat 3 --concurrency 2
```

`cases.json` is an array of `{ case, kind, checks?, units?, session?, version?, content?, evidence?, inputs? }`. `kind` picks the extension's eval adapter: its rubric with its declaration and inputs builder, or, for a rubric not yet declared, its evidence builder. `session` is a recorded session directory whose last (or `version`th) artifact of that kind is judged, with evidence rebuilt by the adapter from the recording as it stood when that version was sealed, through the same builder the live judge uses: `research.report` (fetch log, kept source files, request), `contract.spec` (artifacts, approvals, answered questions, workspace files) and every Sleeper card (the facts step result of the workflow run that sealed it; for an undeclared job, with the news checks dropped when the recorded verdict says the job had no news source, as the live judge drops them). `content` and `evidence` (scope to units), or `inputs` (name to text) for a declared rubric, give them inline for a kind whose adapter cannot rebuild them, such as `github.review`. `checks` (`id` or `criterion.id`) limits the run to the checks being watched, and `units` (ids such as `finding-14`) to those units of an undeclared rubric's evidence, or to those items of the watched checks' scopes of a declared one (an item's id, or its unit's `claim finding-14`), so one finding of a long report is a case of its own. An arm is `label=ref[:decision-model]`: `.` is this tree, any other value is a git ref whose extensions, judge engine and SDK all load from that ref, so an arm sees a change to `packages/judge` or `packages/sdk` as well as one to a rubric. The decision model is `none` or `provider/model`; without one the arm takes `--decision-model`, or none. Without `--arm` it runs one arm, `current=.`. `--escalations` bounds each case's concurrent judge-model calls (4 by default, as in the live judge), so the judge model sees at most `--concurrency` times that at once.

To measure a decision-model change, run two arms on the same tree:

```
... replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --arm base=.:none --arm clef=.:clef/clef --repeat 3
```

It prints a markdown table of per-repeat `met` (a decided fold as 1 or 0; null: no unit or an undecided fold, the check does not count) and decision-model/escalation counts per case and check for each arm, then each arm's median and p90 judge seconds per case, then every case and check whose `met` values differ between arms with each arm's values and the judge's notes, for adjudicating false positives, and last any case an adapter declined with what its recording lacks. Paste it into the PR with the cases' provenance.

## Checking a rollout

Before `compose up` with a new image, run its validator against the prod config, agents and data volume, mounted read-only. It loads every bundle and every session's pinned `agent-def/agent.json` with the deployment-level extensions (unset extension env gets placeholders, nothing is contacted), prints `FAIL <path>: <error>` per failure and `N loaded, M failed`, and exits non-zero on any failure:

```
docker run --rm -v <data>:/data:ro -v <config>:/config/deployment.json:ro -e QUACK_CONFIG=/config/deployment.json -e QUACK_AGENTS_DIR=/app/agents -e QUACK_DATA_DIR=/data <new-image> node packages/runtime/src/validate.ts
```

## Session containers

With `QUACK_SESSION_DRIVER=docker`, each session runs in its own container from `QUACK_SESSION_IMAGE` under `QUACK_SESSION_RUNTIME` (gVisor's `runsc` by default), with only its own directory of the data volume mounted. Two more containers join the sessions on their network:

- **The session network** (`QUACK_SESSION_NETWORK`, default `quack-sessions`) is internal with an isolated gateway, so it has no route out and its bridge has no address on the host. The distributor refuses to start sessions on any other kind of network:
  `docker network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated --subnet 10.213.0.0/24 quack-sessions`
- **The distributor** joins it at a fixed address, which `QUACK_SESSION_DISTRIBUTOR_URL` names with the session port, e.g. `http://10.213.0.2:8081`. Sessions reach models, Exa, Langfuse and the extension apps only through its broker there.
- **The egress proxy** joins it at a fixed address too (`QUACK_SESSION_EGRESS_PROXY`, e.g. `http://10.213.0.3:3128`) and joins an ordinary network for the way out. It runs `ubuntu/squid:6.6-24.04_edge@sha256:8a3baed477e2c282ab8aa5edad442f69873246964f225c5c2ae8364b6610963c` with [`packages/distributor/egress/squid.conf`](../packages/distributor/egress/squid.conf) mounted at `/etc/squid/squid.conf`. It passes any public host on ports 80 and 443, which git clones, dependency installs and the researcher's `web_fetch` need, and refuses private, loopback, link-local and tailnet (100.64.0.0/10) addresses, including names that resolve to them.

Session containers get no DNS: gVisor cannot reach Docker's embedded resolver, so both URLs name their hosts by IP, and the proxy resolves every other name. The driver sets `HTTP_PROXY`, `HTTPS_PROXY`, their lower-case forms and `NODE_USE_ENV_PROXY=1` in each container, with `NO_PROXY` set to the distributor's address, `localhost` and `127.0.0.1`. Through the proxy, Node sends `CONNECT <name>` without resolving the name itself, so the research `web_fetch` address check refuses only IP literals there; squid's `not_public` ACL is what refuses names that resolve to private addresses.

The distributor talks to the Docker Engine at `QUACK_DOCKER_HOST` (default `/var/run/docker.sock`). It makes only these calls: `GET /networks/{name}`, `GET /containers/json`, `POST /containers/create`, `POST /containers/{name}/start`, `POST /containers/{name}/stop`, `POST /containers/{name}/wait` and `DELETE /containers/{name}`. Access to that socket is root on the host.
