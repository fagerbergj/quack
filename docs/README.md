# Quack docs

The operator CLI is described in [cli.md](cli.md) and the QA rig in [qa.md](qa.md).

## Pull requests

A pull request into the trunk needs green CI: typecheck, tests and sloplint. The tools below measure a prompt, guide or judge change when a number helps a decision; none of them is required.

### Comparing agent definitions

`main.ts run` replays the same case under the baseline's agent definition and the candidate's, one arm after the other, on local models:

```
QUACK_LOCAL_API_KEY=... node packages/eval/src/main.ts run items.json ab deployment.json agents/contracts/agent.json --arm base=<trunk ref> --arm cand=. --guides packages/contracts/skills --out ab.jsonl
```

A dataset ending in `.json` is a local file of items, and such a run needs no Langfuse and publishes nothing; any other dataset name is read from Langfuse with the `LANGFUSE_*` variables, and each arm's results are published there as the run `<run-name>-<label>`.

`main.ts dataset items.json <agent> <good|bad|-> <session-dir>...` adds recorded sessions to such a file, and an item can also be written by hand: `{ "id": "status", "input": { "inputs": ["Define a REST contract for GET /status ..."], "pins": {} } }`.

An arm is `label=ref`: `.` is this tree; another ref replays on the agent bundle's directory (`agent.json` and its prompt) as it stood at that ref and, for each `--guides` directory under `packages/`, that directory from the ref over a copy of this tree's packages, so extension code stays this tree's in both arms.

An item's arms run one after another, never at once; `--concurrency` runs that many items together.

Each recorded input is sent once the run before it settles.

The replay answers the run's asks: a question takes the item's next recorded answer when it is one of the options (or free text where the question takes it), else its suggested option; an approval takes the item's next recorded verdict, and the replay stops at the first approval past them. An approve the session holds for a judged version's approve-time checks becomes a rejection by the checks when they fail; when they cannot run it reopens the approval, and the replay answers it again with the next recorded verdict.

`--suggested` drops the recorded answers, and `--verdict approve|stop|reject:<reason>`, repeated, replaces the recorded verdicts; a contracts review round is a rejection whose reason carries the review's `quack-review` block.

Each arm prints a line per item with its scores, each kind's sealed versions (pass, or fail with the gates that failed), and the asks it answered, then a summary per arm; `--out` appends each result with its sealed versions' content and verdicts as JSON lines.

### Replaying the judge

`replay-judge.ts` judges the same recorded cases under a baseline and a candidate on local models:

```
QUACK_LOCAL_API_KEY=... QUACK_CLEF_API_KEY=... node packages/eval/src/replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --decision-model clef/clef --arm base=<trunk ref> --arm cand=. --repeat 3 --concurrency 2 --rows rows.jsonl
```

`cases.json` is an array of `{ case, kind, checks?, units?, session?, version?, content?, evidence?, inputs? }`.

`kind` picks the extension's eval adapter: its rubric with its declaration and inputs builder, or, for a rubric not yet declared, its evidence builder.

`session` is a recorded session directory whose last (or `version`th) artifact of that kind is judged, with evidence rebuilt by the adapter from the recording as it stood when that version was sealed, through the same builder the live judge uses.
The builders are `research.report` (the fetch log and search hits; a session recorded before search hits were logged has none, so its replayed report can fail a link that came only from a search), `contract.spec` (artifacts, approvals, answered questions, workspace files), and every Sleeper card (the facts step result of the workflow run that sealed it; for an undeclared job, with the news checks dropped when the recorded verdict says the job had no news source, as the live judge drops them).

`content` and `evidence` (scope to units), or `inputs` (name to text) for a declared rubric, give them inline for a kind whose adapter cannot rebuild them, such as `github.review`.

`checks` (`id` or `criterion.id`) limits the run to the checks being watched (skipping names an arm's rubric lacks, and failing only if no arm has the check), and `units` (ids such as `finding-14`) to those units of an undeclared rubric's evidence, or to those items of the watched checks' scopes of a declared one (an item's id such as `paragraph-3`, or its unit's `prose_list paragraph-3`), so one item of a long artifact is a case of its own.

Timing claims need full-round judging (no `checks` filter on the case, because a filtered replay skips other checks that share the decision-model lane); pass `--report-checks <id>` to filter the printed table and disagreements while keeping full-round per-arm timing and counts.

An arm is `label=ref[:decision-model][:reuse]`: `.` is this tree, any other value is a git ref whose extensions, judge engine and SDK all load from that ref, so an arm sees a change to `packages/judge` or `packages/sdk` as well as one to a rubric.

The decision model is `none` or `provider/model`; without one the arm takes `--decision-model`, or none.

Every arm judges each case fresh unless it ends in `:reuse`: then, in each repeat, the cases of one recorded `session` are judged one after another in `version` order against an item-reuse store of their own, as the live gate judges a session's drafts, and an answer decided for one version is not asked again for the next.

Without `--arm` it runs one arm, `current=.`.

Each case puts one judge-model call in flight at a time, so `--concurrency` alone bounds the judge model's load.

Arms run case by case, not as blocks: each repeat goes through the cases in order, and each case is judged under every arm in an order shuffled by `--seed` (random when unset, and printed first), so over cases and repeats every arm takes every place about equally and neither load nor the model server's prefix cache favours one arm.

A `:reuse` arm's batch, one session's drafts in version order against one store, takes the place of its first draft and runs whole; the other arms' cases of that session come before or after it, or alongside it under `--concurrency`.

`--rows` appends each case's rows to a JSON-lines file as the case finishes, so a stopped run keeps what it judged, and `replay-judge.ts --table rows.jsonl` prints the table from such a file.

`--budget`, on a run or with `--table`, prints each arm's checks (by rubric and criterion) by escalation rate, escalated / (`clef` + `esc`), after the table, marks any check over 10% `UNHEALTHY`, and then exits non-zero. A decision-only row's line ends with its escalations by cause.
A check declared `decisionModel: false` is marked `declared`, and one an arm without a decision model judged `not-asked`; neither fails the command, while an asked check the decision model settled nothing for (over its cap, or never sure) does.
`node packages/eval/src/escalation-report.ts <dir> [--decision-model clef] [--since <epoch ms>]` prints the same rates per `rubric@version` and check from the prod verdicts in a directory of `quack export` files, skipping files that are not exports, and fails when it finds no verdict; a verdict records each check's escalations by cause, so a line ends with that split and `outage` (items left undecided) is shown but not rated. Only an older verdict without the split, for a check the decision model settled nothing for, shows as `no-clef` and does not fail.

A row is one check of one case under one arm: `met`, its place (`pos`, 1 first), the items the decision model settled (`clef`), the items escalated (`esc`) and the items reused from an earlier draft (`reused`), and the case's judge milliseconds (`ms`) and model requests by model id (`requests`).

`--decision-only` asks only the decision model and never the judge model, so use it to triage a pass-only rework: it counts what escalates and why, and the judge model's accuracy on escalated items is the same in both arms only while both ask an escalation the same way. Across a change to how escalations are asked, such as one item per call against several, that accuracy differs, so a decision-only triage cannot predict the verdicts of a run that spans it. Every arm needs a decision model and no `:reuse`. An escalated item is left unanswered and listed in its row's `items` by cause (`declared`, `unsure`, `pass-only-no`, `over-window`, `refused` when Clef answered the request with a durable error such as a 4xx, or `outage` when Clef was asked and never answered), beside the items Clef passed (`pass`) or failed (`no`), or held unsure on a check declared `confident-only` (`held`, which passes without escalating). Each arm's line gives the causes and the round verdicts as a range: `pass` if the round passes even with every escalated item failing, `fail` if it fails even with every one passing, and `open` otherwise. A round Clef did not answer measures nothing, so its rows are kept and the run stops with an error. `--expect labels.json`, on such a run or with `--table`, reads hand labels `{ case: { unit id: { flag } } }` (as in a `wrong-output/expected.json`) and gives each labelled item's outcome once per check that judged it and repeat, with totals per arm and check: a real failure (`flag: true`) is `caught` when it escalates or Clef's no stands, and `passed-by-clef` when Clef passes it; a false flag is `false-flag-cleared` when Clef passes it. Its judge seconds count Clef alone, and its `met` and verdicts are not a judge's, so final timing and verdict claims still need a full replay. It reads Clef's answers and the escalations off the judge's memo names, so a ref arm from before those names misreports its items or fails.

A replay rebuilds a `contract.spec` version's evidence as the session stood at its seal: the versions, approvals and answers recorded by then, and each file of a git clone in the workspace as the commit the clone's reflog says HEAD held then, since a session goes on pulling after it seals a version.

A file the commit lacks (such as `node_modules`), or one in a clone made after the seal, is read as it is on disk.

A ref through a symlink out of the workspace (an early session's `app -> /app`, the image's own tree) resolves in no replay, as it resolves in no live judge today.

A replayed code gate applies the arm's rubric and contracts code, so it can differ from the verdict prod recorded under older ones: `contract.spec@4` has no `baseline_matches`, and older code counted an approval only once it was recorded, and matched versions and answer links by other rules.

Compare a replay's code gates with prod only where prod judged with the same rubric and code; the model checks compare either way.
A round whose code gate fails returns before any model is asked, its model checks listed as not run, so an arm whose gate fails a case has no model checks to compare on it.

To measure a decision-model change, run two arms on the same tree:

```
... replay-judge.ts --cases cases.json --deployment deployment.json --judge local/qwen3.8-27b --arm base=.:none --arm clef=.:clef/clef --repeat 3
```

It prints a markdown table of per-repeat `met` (a decided fold as 1 or 0; null: no unit or an undecided fold, the check does not count) and decision-model/escalation counts per case and check for each arm.
Then, per arm: its model requests by model, the items the decision model settled, escalated and reused (counts the arm order cannot bias), its median and p90 judge seconds per case overall and by place, and the seed.
Last: every case and check whose `met` values differ between arms, with each arm's values and the judge's notes, for adjudicating false positives, and any case an adapter declined with what its recording lacks.

## Agent bundles

`agents/<name>/agent.json` is closed.

Required keys:

- `name` (`[\w-][\w.-]*`)
- `version` (positive integer)
- `extensions` (name to config object)

Optional keys:

- `status` (`draft` or `promoted`)
- `description` (string)
- `model` (`provider/modelId`)
- `prompt` (a bare file name beside `agent.json`, no `/` or `\`)
- `exec` (`timeoutSeconds` only)
- `retention` (`idleDays` only)

An unknown key, a bad value or a prompt path leaves the bundle unoffered by the distributor, fails `tools/bundle-locks.ts` and fails the validator.

Pinned copies in the data dir are history: they are never checked against this and load through the loader's compatibility shims.

A session runs on the copy it was pinned to, except its `model` and its judge settings (`extensions.judge`), which it takes from its agent's current bundle each time its process starts, so a woken session runs on the current model and judges its next version with the current decision model, while its prompt stays pinned; a session whose agent no longer has a bundle does not start.

The contract is `contracts:ea66d629-d6f9-472e-bc63-0d325dc65296`.

## Session models

A session runs the first of these that is set: its own pick, its agent's `model`, the Settings default, the deployment's `defaultModel`, then the first available model. A pick or Settings default that is no longer available is skipped with a warning. The deployment's `defaultModel` is optional, so a fresh install runs from `QUACK_DATA_DIR` alone.

The available models are each provider's hand-written models, then every other model its `GET {baseUrl}/models` lists, without the decision-model (`systemone`) providers. A provider that cannot list offers only its hand-written ones. The distributor reuses one discovery for 60 s, or for 5 s when a provider could not list, and callers that arrive while one is in flight share it.

- A listed id is a hand-written model when it is that model's name or ends in `/<name>`, as llama-swap lists a peer's models (`jaison/qwen3.8-27b` is `qwen3.8-27b`). The hand-written limits win over what the listing states, and the model runs under its hand-written name.
- A listed id is left out when it matches, in the same way, a model another provider writes (Clef served by the same llama-swap) or an entry in the provider's `hide` list, or when the listing states output modalities without text. Neither OpenAI-style listings nor llama-swap mark embedding models, so hide them by name: `"hide": ["qwen3-embed"]`.
- A model that is not hand-written runs under the id its provider lists, as `provider/<listed id>`, such as `local/jaison/deepseek-v4.1-flash`. Its context window is the listing's `meta.n_ctx`, `context_length` or `max_model_len`. A model with none of these runs with a 32k window and at most 8k output tokens (pi-ai also clamps `max_tokens` to the window less the prompt). Compaction then starts early, and a request the server refuses as over its context (llama.cpp and vLLM wording included) compacts and retries through pi-durable's overflow path.
- `GET /models` returns `{ models: [{ name, contextWindow?, images?, reasoning? }], defaultModel }`. Here `name` is `provider/model`, `contextWindow` is absent when neither the provider nor the deployment states one, and `defaultModel` is what a session runs when neither it nor its agent names a model.
- `GET /settings` and `PATCH /settings` with `{ "defaultModel": "provider/model" | null }` read and set the Settings default. It is kept in `settings.json` in the data dir, and the distributor logs who changed it.
- `PATCH /sessions/{key}` with `{ "model": "provider/model" | null }` sets or clears the session's pick. `GET /sessions/{key}` and the patch's answer return the pick as `model` and the model the next run resolves to as `resolvedModel`, which counts the agent's own model. A model that is not available is refused with 400, which names the ones that are. So is a pick for a session that has no directory on this host.

The pick is stored in the session dir, beside `agent` and outside the pinned `agent-def` snapshot, so setting it never changes the pin. It applies from the session's next run. An idle process restarts at once. A busy one restarts once its run ends, so a run already going keeps its model. A run waiting on a person, sleeping until a wake time, or parked in its outage backoff still counts as going. If the idle sweep stops such a process, its run resumes on the pick. A new Settings default reaches a running session when its process next starts. The capacity gate is the chosen model's provider's. The message box's picker and the Settings page show the model by name, the one the next run resolves to; choosing another sets it, and choosing the Settings model clears the pick, so the session follows Settings again. Its description says whether the model is the session's pick, from Settings or from its agent.

The judge's and extensions' own models (`judgeModel`, `decisionModel` and the like in their settings) are not affected.

## Sessions API change stream

`GET /sessions:watch` is a `text/event-stream` that says which sessions changed, so a client re-reads them through the list API instead of polling. It reads as `GET /sessions` does: a person or the tailnet, or a session credential with the cross-session grant (otherwise 404).

- `event: hello` comes first, with the server's build as `GET /version` answers it: `{"commit": "...", "builtAt": "..."}`.
- `event: changed` carries `{"keys": [...]}`, the keys of the sessions written since the last one. Writes within 250 ms of the first go out as one event.
- `: keepalive` comments go out every 15 s, so the proxies in front keep an idle stream open.

A change is any write the catalog commits (a lifecycle batch, a rename, archive, answer, stop or delete) and any change to a running session's `waiting`, which the distributor's sweep reads from the session. The stream subscribes before it answers, so a client that reads the list once the stream opens misses nothing. A client re-reads on every reconnect, since events sent while it was away are not replayed.

The feed lives in the distributor's process, which is the catalog's only writer; several distributors on one Postgres would need LISTEN/NOTIFY.

## Checking a rollout

Before `compose up` with a new image, run its validator against the prod config, agents and data volume, mounted read-only. It holds every bundle to the agent bundle contract below, then loads every bundle and every session's pinned `agent-def/agent.json` with the deployment-level extensions (unset extension env gets placeholders, nothing is contacted), prints `FAIL <path>: <error>` per failure and `N loaded, M failed`, and exits non-zero on any failure:

```
docker run --rm -v <data>:/data:ro -v <config>:/config/deployment.json:ro -e QUACK_CONFIG=/config/deployment.json -e QUACK_AGENTS_DIR=/app/agents -e QUACK_DATA_DIR=/data <new-image> node packages/runtime/src/validate.ts
```

`QUACK_AGENTS_DIR` may instead name a directory mounted from the host; mount the same directory into the validator. The distributor reads it on every agent listing, so a bundle added there is offered without a restart. The first time the distributor stores a version it records that bundle's content hash at `agent-versions/<name>/<version>/digest` in the data dir, and a new session of that version is refused with a 409 asking for a version bump once the bundle no longer matches. `version.lock` is the repo's CI check; the distributor never reads it.

## Session containers

With `QUACK_SESSION_DRIVER=docker`, each session runs in its own container from `QUACK_SESSION_IMAGE` under `QUACK_SESSION_RUNTIME` (gVisor's `runsc` by default), with only its own directory of the data volume mounted. Two more containers join the sessions on their network:

- **The session network** (`QUACK_SESSION_NETWORK`, default `quack-sessions`) is internal with an isolated gateway, so it has no route out and its bridge has no address on the host. The distributor refuses to start sessions on any other kind of network:
  `docker network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated --subnet 10.213.0.0/24 quack-sessions`
- **The distributor** joins it at a fixed address, which `QUACK_SESSION_DISTRIBUTOR_URL` names with the session port, e.g. `http://10.213.0.2:8081`. Sessions reach models, Exa, Langfuse and the extension apps only through its broker there.
- **The egress proxy** joins it at a fixed address too (`QUACK_SESSION_EGRESS_PROXY`, e.g. `http://10.213.0.3:3128`) and joins an ordinary network for the way out. It runs `ubuntu/squid:6.6-24.04_edge@sha256:8a3baed477e2c282ab8aa5edad442f69873246964f225c5c2ae8364b6610963c` with [`packages/distributor/egress/squid.conf`](../packages/distributor/egress/squid.conf) mounted at `/etc/squid/squid.conf`. It passes any public host on ports 80 and 443, which git clones, dependency installs and the researcher's `web_fetch` need, and refuses private, loopback, link-local and tailnet (100.64.0.0/10) addresses, including names that resolve to them.

Session containers get no DNS: gVisor cannot reach Docker's embedded resolver, so both URLs name their hosts by IP, and the proxy resolves every other name. The driver sets `HTTP_PROXY`, `HTTPS_PROXY`, their lower-case forms and `NODE_USE_ENV_PROXY=1` in each container, with `NO_PROXY` set to the distributor's address, `localhost` and `127.0.0.1`. Through the proxy, Node sends `CONNECT <name>` without resolving the name itself, so the research `web_fetch` address check refuses only IP literals there; squid's `not_public` ACL is what refuses names that resolve to private addresses.

The distributor talks to the Docker Engine at `QUACK_DOCKER_HOST` (default `/var/run/docker.sock`). Access to that socket is root on the host, so the distributor reaches it only through the docker gate, making only these calls:

- `GET /networks/{name}`
- `GET /containers/json`
- `POST /containers/create`
- `POST /containers/{name}/start`
- `POST /containers/{name}/stop`
- `POST /containers/{name}/wait`
- `DELETE /containers/{name}`

The **docker gate** (`node packages/distributor/src/docker-gate.ts`, from the same image) runs in its own container. It mounts the real socket and serves `QUACK_GATE_LISTEN` (a TCP port, default `2375`, or a socket path); the distributor's `QUACK_DOCKER_HOST` names it, e.g. `tcp://quack-docker-gate:2375`. It reads the same `QUACK_SESSION_IMAGE`, `QUACK_SESSION_RUNTIME`, `QUACK_SESSION_NETWORK` and `QUACK_DATA_VOLUME`/`QUACK_HOST_DATA_DIR` as the distributor, and answers 403 to anything but the calls above:

- a list must filter on the `quack.session` label, and only the session network can be read;
- a create must use the session image, runtime and network, drop all capabilities, mount only one session directory (`quack-session-<dir>` mounts `<dir>`), and carry only the `quack.session` label. Its body may hold only the keys the driver sends, so `Privileged`, `CapAdd`, `Devices`, the host namespaces and extra networks are refused;
- start, stop, wait and delete reach a container only when it has the `quack.session` label, by the id the gate looked up.
