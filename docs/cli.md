# CLI

`quack` is a one-binary CLI and server. There is no TUI: `-p`, `chat send`, and `chat show` are the interface, and their pause/failure exit codes (`0` answered, `1` failed, `2` paused on a question) make them pipeable and scriptable. Every command also runs against a running server over HTTP + SSE (`--server`, or the active one from `quack server add`/`use`) - there's nothing local-only about it except `server run` itself. With neither, the CLI (except `ledger list` and `ledger export`) boots a server in-process from `$QUACK_CONFIG` or `./quack.yaml` for that one command; when that config's session store is postgres (likely shared with a running server), it says so on stderr, since a second engine is then working on the same database.

Every command has its own `--help`; this page is the map. An unknown subcommand (`quack chat lst`) exits 1 with a suggestion. Errors print the server's own message after `Error:` on stderr, without the request's method, URL or HTTP status.

## Shell completion

`quack completion <bash|zsh|fish|powershell>` generates a completion script. Chat, node, memory, and server ids/names complete dynamically (a live call against the target server or local config); `sandbox --agent` completes from the local `quack.yaml`.

## Getting configured

| Command | Does |
| --- | --- |
| `quack init` | Onboarding wizard: run a server locally (writes `quack.yaml`; the CLI then runs it in-process, no server registered) or register a remote one someone else runs. |
| `quack server init` | Just the config wizard - LLM provider, endpoint, model roles, optional features, stores. Writes `quack.yaml` without touching the client registry. `--answers <file.yaml>` skips the wizard entirely for a headless setup (Dockerfile, CI, Ansible) - a YAML file of the same fields the wizard asks for (see `InitAnswers` in `internal/cli/emit.go`); unset fields fall back to the same environment variables the wizard prefills from. |
| `quack server use <name>` / `add <name> <url>` / `list [--json]` / `remove <name>` | Manage the set of servers this CLI knows about and which one is active. `add` activates the server if it's the first one registered. |
| `quack server validate [--json] [--skip-extensions]` | Load and validate a `quack.yaml` without starting the server; also resolves plugins and lists each plugin's seeded agents and shapes, warns about each `bundle:`-less, non-optional override no plugin seeded (boot drops it; `dropped_overrides` in `--json`), and builds every enabled extension through its Factory as boot does, reporting each one's error. That needs what boot needs: each enabled extension's secrets and key files, and a creatable data dir. The Factory gets a throwaway data dir, since some open their stores eagerly; the real data dirs are never created or touched. Disabled extensions are listed, not built. It then checks `decisions.points` against the core points and the enabled extensions' declared ones, as boot does. It also lints each configured agent's `rubric.yaml` and fails on any issue (see [Measuring the judge](configuration/judge-measurement.md#rubric-lint)). `--skip-extensions` checks structure only. |
| `quack server login <name> --issuer <url> --client-id <id>` | Log in to a registered server that requires [OIDC auth](configuration/auth.md#cli-login-quack-server-login), via the authorization code flow with PKCE (needs a local browser - doesn't work headless/over SSH). |

Once logged in, `quack chat`/`quack api`/`-p` attach the stored access token to every request against that server automatically (refreshed silently as it nears expiry) - nothing else to pass on the command line.

## Running the server

`quack server run` runs the REST + MCP API and the embedded SPA in the foreground (`--config`, `--port`). See [deployment shapes](configuration/deployment.md) for the local-vs-Docker-vs-remote options.

## One-shot prompts

```bash
quack -p "Research the best time to visit Dublin"
```

Prints the answer and exits. Flags: `--events` (also print the pipeline trace - plan, node lifecycle - to stderr), `--attach` (attach a file, e.g. an image, repeatable), `--json` (one JSON result object instead of plain text).

## Chats

A chat is a session; a message on it kicks off a run.

| Command | Does |
| --- | --- |
| `quack chat new` | Create a chat, print its id. |
| `quack chat send <id> "<msg>"` | Send a message (also how you answer a paused question). An archived chat is read-only: the send is refused until you `unarchive` it, as `chat node retry` is. A question with choices lists them, numbered, under its `question:` line (`options` in `--json`). |
| `quack chat show <id> [-f]` | Status snapshot - id/title/status (plus `archived: yes` when archived)/pending question and its choices, node table, last answer; `-f` follows a live run. Applies the same 0/1/2 exit-code contract as `chat send`/`-p` (`--json` too). |
| `quack chat list [--status idle\|running\|needs_input\|failed] [--archived exclude\|include\|only]` | List chats with their status. `--archived` defaults to `exclude`; `only` is the CLI's one path to a chat archived in the web UI. |
| `quack chat export <id>` | Export a chat transcript. |
| `quack chat stop <id> [--json]` | Stop a chat's active run; says whether a run was actually stopped or none was active (`action` `stopped` or `none` in `--json`). |
| `quack chat delete <id> [--json]` | Delete a chat (irreversible); a live run is cancelled and given up to 15 s to finish first, else the server answers 409 and keeps the chat. An unknown id is an error, not a silent success. The confirmation prompt goes to stderr; a non-interactive stdin without `-y` errors instead of silently declining. |
| `quack chat rename <id> <title>` | Rename a chat. |
| `quack chat archive <id>` / `unarchive <id>` | Archive or unarchive a chat. |
| `quack chat artifact list <id>` | List a chat's artifacts and their revision history. |
| `quack chat artifact download <id> <name> [--revision N] [-o file\|-]` | Download one artifact revision's bytes (default: the artifact's own name, sanitised to a bare local filename). `-o -` streams to stdout instead of writing a file. |

`chat send`, `-p` and `chat show -f` follow a run through a server restart. A deploy pauses the run without ending it, and boot resumes it, so when the stream drops before the run's `done`, the CLI prints `connection lost; reconnecting…` on stderr. It then reconnects with backoff from the last event it saw, for about 3 minutes per outage, and follows the resumed run to its end; the exit code reflects that end. If the server stays down past that, the command exits `1`, and `quack chat show <id> -f` picks the run up again.

## Node control

Mid-run control over one node in the active DAG (`quack chat node <verb> <chat-id> <node-id> ...`); every verb takes `--json`, printing `{"chat_id","node_id","action","message"}`. A chat or node that doesn't exist is an error naming which one is missing.

| Verb | Does |
| --- | --- |
| `stop` | Stop a running node; the rest of the run continues. |
| `pause` | Suspend a running node, keeping its accumulated work (resumable); the nodes that depend on it wait. |
| `resume` | Resume a paused node; the node carries on in its saved session (an ACP node reloads it and is re-sent its task), then its waiting dependents run. |
| `queue <message>` | Queue a message for a running node, delivered into a worker round in flight at once, else at the node's next turn boundary. |
| `queue-edit <message-id> <text>` | Edit a not-yet-delivered queued message. |
| `queue-remove <message-id>` | Remove a not-yet-delivered queued message. |
| `edit <task>` | Edit a not-yet-started node's prompt; a node that has started or finished refuses the edit. |
| `retry` | Re-run a finished node (done/failed/cancelled) and everything downstream of it. |

## Recording and eval

Runs can be recorded to the ledger and re-driven later - the basis for regression-checking a prompt or model change against real traffic.

| Command | Does |
| --- | --- |
| `quack ledger list` / `export <chat-id>` | List chats with a recording on the server; download one as a bundle for `eval` or a fixture. Both need `--server` or an active server and never boot one in-process. |
| `quack ledger show <chat-id> [--from-seq N]` | Print a chat's raw ledger entries (server-side, from the local quack.yaml's stores). `show`, `recover` and `rebuild` (like `experiment run`) never talk to a server, so they refuse `--server` rather than ignore it; `ledger export --server <url>` fetches a recording from a server. |
| `quack ledger recover [chat-id] [--dry-run] [--json]` | Settle intents whose projection write is missing (the same pass the server runs at boot); `--dry-run` reports only. |
| `quack ledger rebuild <chat-id> [--dry-run] [--json]` | Reconcile a chat's artifact metadata and SSE table against the ledger fold. |
| `quack decisions report [--since 7d] [--point <id>] [--chat <id>...] [--json]` | Per decision point, compare the decision model against quack's own outcome: call count, unavailable calls, coverage, agreement, confident disagreements, latency, input tokens, and a top-vs-baseline confusion table. See [`docs/configuration/decisions.md`](configuration/decisions.md#reading-the-data). |
| `quack decisions export --langfuse [--since 7d] [--point <id>] [--chat <id>...]` | Upsert each point's decisions as the Langfuse dataset `decisions/<point id>` (input = state and questions, expected output = quack's baseline), with a run `<handler>@<quack version>` carrying the model's answer and scores. Reads credentials from the local `quack.yaml` (`prompts.store`); safe to re-run. |
| `quack eval [--from-server <url>]` | Re-run a recorded conversation live with a swapped model and compare judge scores. `--from-server` names where to fetch the recording when the argument is a chat id (default: the global `--server`, else the active registered server); the eval run itself always runs from your local `quack.yaml`. |
| `quack judge replay <chat-id-or-bundle.zip> [--node <id>] [--round N] [--rubric <path>] [--from-server <url>] [--deterministic-only] [--repeat N] [--out <file>] [--verify] [--clone <dir>] [--json]` | Re-grade a recording's already-judged rounds under the working-copy rubric, without re-running the agent or the original judge call - proves a rubric or gate-code change against real traffic in seconds instead of a rig cycle. A chat id's recording comes from `--from-server`, else `--server`, else the active registered server. Exits 2 when any criterion's pass/fail flips, 1 when a round could not be replayed or a filter matched nothing, 0 otherwise. `--deterministic-only` replays only the code-owned criteria and never touches a model; against a server the report also carries the shadow locate tier's rows (each specific in the answer looked for in its cited page's stored text), which never gate; `--repeat N` judges each round N times and reports every judge-scored criterion's spread (mean, sd, pass rate, agreement), the judge's own noise on a fixed answer, plus an overall consistency summary; `--out <file>` also writes the `--json` report to a file to store and diff; `--clone <dir>` points replay at a git checkout of the reviewed head (cloned at the base branch, then checked out at the head), so a reviewer round's judge gets the diff under review, the finding excerpts and live's read tools. See [`docs/configuration/trust-gate.md`](configuration/trust-gate.md#judge-replay). |
| `quack judge score-golden <dir> [--deterministic-only] [--out <file>] [--json]` | Grade a labeled golden set (`config.json` plus one JSON file per item) through the live gate's own functions and report false-pass and false-fail rates per criterion and overall against the human labels. Exits 2 when a rate exceeds the set's limit, 1 when an item errored or nothing was scored. `--deterministic-only` scores the code-owned criteria with no model and no `quack.yaml`, skipping items marked `needs_live_model`; without it the local `quack.yaml`'s `gates.judge` model grades those. See [Measuring the judge](configuration/judge-measurement.md#golden-set). |

## Datasets and experiments

Export recorded runs to a Langfuse dataset, then re-run an agent against that dataset outside any live GitHub event - the basis for comparing prompt versions in Langfuse's evaluation/comparison views.

| Command | Does |
| --- | --- |
| `quack dataset export --dataset <name> (--chat <id> \| --repo <owner/repo> \| --since <date>) [--agent <name>] [--limit N] [--server <url>]` | Export gated code-reviewer, synthesizer, web-researcher and trend-scout node runs as Langfuse dataset items (creating the dataset if needed). `--since` alone scans every chat updated since that date, which is how runs in chats with no repo are found; with `--chat` or `--repo` it narrows those. `--agent` keeps only that agent's runs. Each turn's run of a node is its own item, so a recurring job's chat yields one per turn; `--since` and `--repo` scans skip `ext:experiment:` replay chats. A pull request review's item also carries `input.setup`: the repository, base branch and head branch it was dispatched with, and the head commit its node started on; the summary counts the items exported with one. An item whose task lists input artifacts (its `<artifacts>` block) carries each one's content at the listed revision in `input.artifacts`, and `experiment run` writes them into the replay's chat before the node starts. An item stays under 4 MB, inside Langfuse's 4.5 MB request limit: artifacts go in smallest first, and one that doesn't fit is left out whole, never truncated, named in `input.skipped_artifacts` and logged with its size. The replay's run item then carries `skipped_artifacts` in its metadata, so a comparison can exclude a run that missed inputs. Idempotent: re-exporting the same node run upserts the same item instead of duplicating it. Without `--server` it reads the local quack.yaml's stores; `--server <url>` reads the same chats, recordings and artifacts over that server's API instead (the active registered server is never used implicitly), so exporting from a remote deployment needs only Langfuse credentials in the local quack.yaml; a server too old to report a chat's `setup` exports its review items without a clone. |
| `quack dataset seed --from <dataset> --dataset <name> --mirror <path> [--limit N]` | Copy exported code-reviewer items into `--dataset` with one known defect planted in each PR, so `experiment run` scores the reviewer's recall in code (`seeded_hit`). The defect lands on a Go line the PR added, outside test files, picked deterministically from the item id among the subtlest tier the PR offers. First tier: `and_or` swaps `&&` and `\|\|`; `off_by_one` drops a binary `+ 1`/`- 1` or turns an index, slice or `i < len(x)` bound into `len(x)-1`; `drop_defer` deletes a `defer x.Close()`-style zero-argument method call. Second: `flip_eq` (`==`/`!=`), `boundary` (`<`, `<=`, `>`, `>=`), `drop_not`, `return_nil` (a trailing `return ... err`). Last: `drop_err_guard` deletes an added `if err != nil { ... }` block whole. The PR's head commit is rewritten in place with the same parents, author and message, so the history shows no extra commit and a re-seed upserts the same item. `--mirror` is a bare mirror of the items' repository, cloned from the first item's repo when absent and fetched for a missing commit. Each seeded item gets its own repository, `<mirror dir>/items/<item id>.git`, holding only the PR's base and head branches and borrowing the mirror's objects; its `input.setup` points there over `file://` at the seeded head, its task and input artifacts carry the seeded sha, its `files` artifact the regenerated patch, and `metadata.seeded` records `{path, line, operator, original, mutated}`. Replaying it needs [`workspace.file_repos`](configuration/workspace/index.md#file_repos) naming the mirror's directory in the local `quack.yaml`. An item with no pinned head, no `pull` artifact base or no mutable added line is skipped and named in the summary. |
| `quack experiment run --dataset <name> --agent <name> [--prompt system/<agent>@N] [--run-name <s>] [--limit N] [--concurrency N] [--model <provider>/<model>] [--judge-model <provider>/<model>] [--keep-workspaces] [--json]` | Run `--agent`'s node alone on every item's recorded task in `--dataset`, reporting each as a Langfuse dataset run item linked to the node's trace. The trace gets one numeric score per judge criterion of the gate's final round, `gate_passed`, and the `cost.*` scores below; the run's description and metadata name the prompt version the node actually resolved and the worker and judge models it actually called (`worker_model`, `judge_model`). `--prompt system/<agent>@N` pins the run's resolver to that exact Langfuse version, a plugin agent's bundle prompt included, and an item whose node ran any other prompt is an item error, not a scored run. `--concurrency N` (default 1) runs up to N items at once, and `all` or `0` runs every item at once; the summary stays in dataset order. `--model` and `--judge-model` rebind the agent's worker and the gate judge for this run only; see [Screening on a hosted model](#screening-on-a-hosted-model). Each item's workspace (its clone and per-node dirs) is deleted once the item is read back, whatever its outcome, and startup deletes `ext-experiment-*` workspaces over six hours old that a killed run left; `--keep-workspaces` keeps them all for debugging. Exits non-zero when any item errored. |
| `quack prompt push <agent>` | Upload `<agent>`'s bundle `prompt.md` to `prompts.store` as a new `system/<agent>` version without the `production` label, and print `system/<agent>@N` to pin as an experiment's baseline. |

### Measuring a prompt change

A prompt change is proven by running the same recorded inputs through the current version and the candidate, then comparing the two dataset runs in Langfuse. Datasets and experiments are kept for the high-traffic prompts only: code-reviewer, synthesizer, web-researcher and trend-scout. Give each one a dataset of at least 20 recorded cases that includes known-bad runs (ones the gate failed or sent back for revision), so a regression has something to show up on. Langfuse is the proving ground and the repo is the record: production serves the bundle's `prompt.md`, and a candidate that wins on Langfuse is committed back to it. The example below pushes the synthesizer's current prompt as the baseline version 1, then measures a candidate saved in Langfuse as `system/synthesizer` version 2 against it. Create the candidate in the Langfuse UI without the `production` label, so live traffic keeps the baseline.

Keep datasets and experiment runs in their own Langfuse project. In the local `quack.yaml` the commands run from, add a second `kind: langfuse` store holding that project's keys and name it in `experiments.store`; prompts still resolve from `prompts.store`, so the pinned versions are production's. A run item links a trace only within its own project, so the local run's traces must go there too: point an `observability.otel.exporters` entry with `signals: [traces]` at `<langfuse url>/api/public/otel` and export `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic $(printf %s '<experiments public key>:<secret key>' | base64)"`.

```yaml
stores:
  langfuse:     { kind: langfuse, url: https://langfuse.example, public_key: pk-prod, secret_key: sk-prod }
  langfuse-lab: { kind: langfuse, url: https://langfuse.example, public_key: pk-lab, secret_key: sk-lab }
prompts:     { store: langfuse }      # prompt versions: the production project
experiments: { store: langfuse-lab }  # datasets, runs and scores: the experiments project
```

```sh
# 1. A dataset of recorded synthesizer runs: input is each node's task, expected output its answer
quack dataset export --dataset synth-ab --agent synthesizer --since 2026-09-01 --limit 30
# 2. The bundle's prompt.md as the baseline version; it prints system/synthesizer@1
quack prompt push synthesizer
# 3. Baseline and candidate over the same items, each pinned to one version
quack experiment run --dataset synth-ab --agent synthesizer --prompt system/synthesizer@1 --run-name synth-v1
quack experiment run --dataset synth-ab --agent synthesizer --prompt system/synthesizer@2 --run-name synth-v2
```

Each item runs only the agent's node, on an in-process server built from the local `quack.yaml`: a one-node bound workflow whose task is the item's recorded task, which already carries the upstream nodes' answers. Nothing is planned, no upstream node re-runs, and delivery is denied, so a replayed node never posts or pushes. Every run item links to the node's own trace, and that trace carries these numeric scores:

| Score | What it is |
| --- | --- |
| one per rubric criterion (`grounded`, `citation_quality`, `cites_sources`, ...) | The gate's final judged round, on the scale `agents/<agent>/rubric.yaml` declares for that criterion (0-3 for the synthesizer's judged criteria, 0-1 for its code-owned `cites_sources`, `specifics_cited` and `specifics_supported`); the judge's explanation is the score's comment. |
| `gate_passed` | 1 when the gate passed the node, else 0; averaged over a run, the pass rate. |
| `cost.input_tokens`, `cost.output_tokens`, `cost.cached_tokens` | The node's worker token totals across its draft and every revision, judge calls excluded. |
| `cost.model_turns`, `cost.tool_calls`, `cost.rejected_tool_calls` | Model calls the worker made, tool calls it issued, and calls answered with an error (an unknown tool, arguments that failed the schema, a failed read). |
| `cost.judge_rounds` | Judge rounds the node needed; more than one means the gate sent the draft back for revision. |
| `cost.wall_time_s` | The node's wall time in seconds, judge rounds included. |
| `seeded_hit` | Seeded items only: 1 when a final inline review comment lands in the planted defect's file within 3 lines of it, else 0; averaged over a run, the reviewer's recall. Read from the chat's final `code_review` record and its findings, never from prose. The comment names the operator and location. |
| `review_comments` | Seeded items only: how many inline comments the final review holds, so a recall gain bought by commenting everywhere shows up. |

A Langfuse v4 deployment in `events_only` mode answers the v1 read endpoints (`/api/public/traces`, `/api/public/scores`, a dataset's runs) with 404, so check a run from the UI, `/api/public/v2/observations?traceId=<trace>`, or the `scores-numeric` view of `/api/public/v2/metrics` filtered on `traceId`. Compare the runs on the dataset's runs view in Langfuse, which averages each score per run, then open the two runs side by side to read individual items. Read quality first: `gate_passed` and each criterion's average should hold or rise. Then cost: with quality equal, fewer tokens, turns, rejected calls and judge rounds make the candidate the better prompt. A rise in `cost.judge_rounds` with an unchanged `gate_passed` means the candidate passes only after more revisions. Because the synthesizer's generations carry the resolved prompt name and version, the prompt's own metrics in Langfuse also split by version.

Before trusting a criterion's difference, measure the judge's noise on it. `quack judge replay <chat> --from-server <url> --repeat 5` re-judges recorded rounds of a dataset item's source chat (the item's `chat_id` metadata) five times over the same answer and reports each criterion's sd and pass/fail agreement; see [Measuring the judge](configuration/judge-measurement.md#judge-consistency). A baseline-to-candidate gap smaller than that spread is judge noise, not a prompt effect. Repeated runs per item and accuracy against golden labels are not part of the default loop; add them for an experiment whose result is a close call.

Once a candidate wins, promote it by hand: copy its text from Langfuse into the agent's bundle `prompt.md` (`agents/<agent>/` or, for a plugin agent, `extensions/<plugin>/plugin/agents/<agent>/`) in a PR whose description cites the baseline and candidate run names, `synth-v1` and `synth-v2` above.

### Screening on a hosted model

A code-reviewer item takes about 15 minutes on a local model, so screen candidates on a cheap hosted model with every item in parallel, then confirm the winner on the production model with a plain run. `--model <provider>/<model>` points the agent's worker, an ACP agent's pi included, at a `models:` entry; `--judge-model` does the same for `gates.judge`, which also serves the ACP permission judge. The ref splits at its first slash, so `openrouter/deepseek/deepseek-v4.1-flash` is provider `openrouter`, model `deepseek/deepseek-v4.1-flash`, and the model must be registered under that provider. Add both to the local `quack.yaml`; `extra_body.provider` pins every call, pi's included, to one OpenRouter upstream (see [Providers](configuration/models.md#providers)):

```yaml
providers:
  openrouter:
    kind: openai
    endpoint: https://openrouter.ai/api/v1
    api_key: ${OPENROUTER_API_KEY}
    extra_body:
      provider: { order: [Together], allow_fallbacks: false }
models:
  deepseek/deepseek-v4.1-flash:
    provider: openrouter
    cost:                  # USD per million tokens: set the pinned upstream's actual rate
      input_per_mtok: 0.0152
      output_per_mtok: 0.60
```

Launch the baseline and candidate arms together, each with every item in flight:

```sh
M=openrouter/deepseek/deepseek-v4.1-flash
quack experiment run --dataset code-reviewer-ab --agent code-reviewer --prompt system/code-reviewer@1 \
  --run-name cr-v1-flash --concurrency all --model $M --judge-model $M &
quack experiment run --dataset code-reviewer-ab --agent code-reviewer --prompt system/code-reviewer@2 \
  --run-name cr-v2-flash --concurrency all --model $M --judge-model $M &
wait
```

`--concurrency all` (or `0`) starts every item at once and lifts the run's clone bound to match. Nothing else in quack caps a run on a provider without `limits:`: admission (`dag.Admission`, worker and judge alike) queues only on a model's `limits:` and a provider's `limits.active`, so the llm-swap provider's caps still hold for its own models and never touch these calls. OpenRouter's own rate limits still apply. Every native model call, the judge's included, retries a 5xx or 429 with backoff for about a minute, so a pinned upstream's 502 burst doesn't fail the item.

The run is self-contained: its in-process server drives every item, and nothing waits on a wall clock except a per-item bound of two hours that surfaces a hung item. It may share a live server's database and workspace volume, because a server booting on that database skips `ext:experiment:` chats instead of settling them, sending their turns again or deleting their clones. A run's clone and `check_setup` are bounded by `workspace.check_timeout_seconds`, not the 60-second tool-call bound. If an `experiment run` process dies, its chats stay marked active, and no server ever resumes them; the next run deletes their workspaces once they are six hours old, well past the two-hour item bound, so another arm's live items are never touched.

`cost_usd` on each call is priced from the model's `cost:` entry, not the cost OpenRouter reports back, because pi drops that figure; a model without `cost:` records tokens but no cost. Quack has no cached-input tier, so cached tokens bill at `input_per_mtok`. Settings tied to the model rather than the run stay as `quack.yaml` sets them: the agent's `context_window` (it must fit the model's `context_window` when one is declared), `gates.judge.thinking_level`, and the model's `effort`. Recall still embeds on the `default` provider's embedder. A Langfuse prompt version whose config binds `model:` still wins: `system/<agent>` over `--model` for a native agent, and `system/judge` over `--judge-model`. The run's `worker_model` and `judge_model` metadata show which model each actually called.

Code-reviewer and trend-scout are plugin agents: production always serves their bundle `prompt.md`, and only an experiment's `--prompt` replaces it. An item with `input.setup` first clones that repository and checks out the head branch at the recorded commit, fetched by its sha, so the replay reviews the code the original review saw even after the branch moved on or was deleted. Items exported before head commits were recorded have no `input.setup` and replay with no clone, so re-export them. code-reviewer exists only when `extensions.github` is configured in the local `quack.yaml`; with no GitHub App, use the credential-free block in [QA mocks](qa-mocks.md#github-mock) with the mock running, since the extension asks it for each clone's token. A public repository clones anonymously, as git sends a token only when the host asks for one; a private one needs the App's installation or a `workspace.git_credentials` token for its host. A replayed node has none of the pages its upstream nodes fetched, so the code-owned page checks (`cites_sources`, `specifics_supported`) score low in both runs alike: compare a synthesizer's judged criteria, not those.

## Memory

Browse or invalidate what quack has remembered (memory lifecycle design doc); `forget` is the CLI verb for the manual-delete remediation path a memory-poisoning incident needs.

| Command | Does |
| --- | --- |
| `quack memory list [--bucket <b>] [--q <query>] [--tier <t>] [--sort <s>] [--limit N] [--include-invalidated]` | List or (with `--q`) embedding-search memories. With no `--limit`, auto-pages through the whole store rather than stopping at the server's default page. |
| `quack memory show <memory-id>` | Show one memory's full detail (votes/tier/last-recalled) - `GET /api/v1/memories/{id}`, a direct per-id lookup, not a store-wide scan. |
| `quack memory forget <memory-id> [--reason <text>] [--json]` | Invalidate (soft-delete) one memory. |
| `quack memory sweep [--dry-run] [--dedupe [--apply]]` | Run the forgetting-rule sweep on demand (epic #1255 P3); `--dry-run` reports per-rule matches without invalidating anything. `--dedupe` runs the per-bucket similarity dedupe instead (#1269); `--apply` actually writes the merges (default reports clusters only). |
| `quack memory rescope [--apply]` | Move role:\* memories with a resolvable GitHub-origin chat into their repo:\* bucket (#1262); dry run by default. |
| `quack memory stats [--weeks N]` | Weekly recall precision/vote/recall counts plus live/invalidated/never-recalled/no-votes/unsupported-verified points per scope; defaults to 12 weeks. |

## Sandbox

`quack sandbox` enters or probes the real agent jail - the same caps, argv wrapping, and spawn env an ACP agent gets, which is what makes it useful for reproducing "works for me, fails in the jail" bugs.

| Command | Does |
| --- | --- |
| `quack sandbox` | Enter the jail interactively. |
| `quack sandbox run "CMD ARGS"` | Run one command inside the jail and exit. |
| `quack sandbox info [--json]` | Print the resolved jail (mode, cwd, tmp, home, grants, env) without running anything. |
| `quack sandbox check [--json]` | Run the built-in jail probes; non-zero exit on any FAIL. `--json` emits the same rows as a machine-readable array. |

See [`docs/sandbox-cli.md`](sandbox-cli.md) for the detail.

## Extensions

| Command | Does |
| --- | --- |
| `quack extension restart <name>` | Restart one out-of-process extension's child on its re-resolved binary; with `exec: release` that is the release at its plugin row's current pin. The old child finishes in-flight calls; the server and other extensions keep running. See [Releasing an extension without a quack release](extensions/dev-loop.md#releasing-an-extension-without-a-quack-release). |

## Misc

`quack version` prints the version. `quack git-askpass` is a helper quack invokes for itself during git operations - not something you run directly; it reads the credential from `QUACK_GIT_ASKPASS_USERNAME`/`QUACK_GIT_ASKPASS_TOKEN`, set by the calling process for that one invocation, and answers only a prompt for the host in `QUACK_GIT_ASKPASS_HOST`.

## Raw API access

`quack api [method] <path>` is a `gh api`-style passthrough to the REST API - `quack api /health`, `quack api POST /api/v1/chats -d '{"system_prompt":"..."}'`, `-d @body.json` or `-d -` for stdin. Targets the active server (or `--server`); with neither configured, it runs the duck in-process. See [`docs/api.md`](api.md) for the full REST/MCP/A2A surface this rides on.
