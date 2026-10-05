# Decision points

A decision point is a named intercept point in quack. When it is enabled, quack hands the point's evidence (text an LLM already wrote) to a handler, and the point's policy acts on the handler's answer or only records it. The one handler kind today is a System One decision model: one forward pass over typed questions, returning calibrated probabilities instead of generated text. Any endpoint speaking the Clef/Kev/Jev `POST /v1/systemone` shape works.

Every point is off unless `decisions.points.<id>.enabled` is true. A disabled point makes no call at all. An enabled point never blocks or fails a run: if the handler is down, cold, slow, or the input is over its cap, the point gets "no decision" and fails open to quack's own logic, unless it is a guard declared `fail: closed`.

```yaml
decisions:
  handlers:
    clef:
      kind: systemone          # the only kind; default systemone
      url: http://llm-swap-media:11436/upstream/clef-27b   # quack appends /v1/systemone
      model: clef              # the request's model field; default: the handler's key
      timeout: 5s              # whole call, retry included; default 5s
      max_input_tokens: 8192   # skip inputs well over the server's cap; default 8192
  points:
    plan.accept:
      enabled: true
      handler: clef
      mode: observe            # observe | guard | decide; default observe
      act_at: 0.9              # top-answer probability guard/decide need to act; default 0.9
      timeout: 3s              # optional: a tighter cap than the handler's
      fail: open               # open | closed (guard only); default open
      questions:               # optional: override a built-in question's text
        accept:
          instructions: Should an independent reviewer accept this plan step?
          # criteria: {true: ..., false: ...}   # noul; choice takes {option: description}, score a list
    memory.extract: { enabled: true, handler: clef }   # mode defaults to observe
    answer.accept: { enabled: true, handler: clef }
    research.source: { enabled: true, handler: clef }
    "ext:github/review.verdict":   # an extension's declared point
      enabled: true
      handler: clef
      mode: observe
```

## Handlers

`kind: systemone` sends `url` + `/v1/systemone`. The call is retried once on a connection error, a 429 or a 5xx (Clef answers 503 when one batch runs out of GPU memory). A 413 (input over the server's token cap) and a 422 or 400 (a request the server rejects) are never retried. A 422 or 400 is also logged at warn, since it means quack sent a malformed question.

llama.cpp (`llama-server` with a decision-model GGUF) is a supported backend. It reports an over-cap input as HTTP 500 "input (N tokens) is too large to process" and quack treats that like a 413 (not retried), rejects schema errors with 400, and returns no `latency_ms`, so the record omits `server_ms`. Set `max_input_tokens` to the server's `-ub`. llama.cpp serves these requests one at a time, so set `timeout: 20s`.

`timeout` bounds the whole call, and a point's own `timeout` can only shorten it. Keep both short: a llama-swap upstream that is still loading (Clef's cold start is about six minutes) blocks until the timeout, and the point then records `unavailable`.

`max_input_tokens` is a pre-call guard. It estimates tokens at six bytes each, above English's average of about four, so it skips only inputs well over the cap. A borderline input goes to the server, whose 413 is authoritative.

## Points, namespaces and modes

`points` is keyed by point id. Core points have unprefixed ids (`plan.accept`) and are registered in code, each with its questions, a primary question, the modes its caller implements, and the step a `decide` outcome would replace. The `ext:` prefix is reserved for extensions: an extension declares its points in its plugin (the SDK's `DecisionPoints`: questions, primary question, restrictive answers, modes; no modes means `observe` only), each becomes `ext:<plugin>/<name>`, and an extension can only evaluate its own declared points. At boot, after the enabled extensions are built, a point that is neither registered nor declared fails startup, as does an enabled point that sets a mode its caller doesn't implement, overrides a question it doesn't ask, or fails closed without a restrictive answer. For an `ext:` id the error says whether the extension is not enabled, declares no points, or declares other names, and lists the ones it does declare. The one exception is a disabled `ext:` entry for an extension that is not enabled: it is accepted, since it cannot be told apart from a typo there. `quack server validate` runs the same check after building the enabled extensions; `--skip-extensions` skips it.

| Mode | What the caller does with the primary question's top answer |
| --- | --- |
| `observe` | Nothing. The decision is recorded next to quack's own outcome. |
| `guard` | Applies it only when it is confident (top probability ≥ `act_at`) and is one of the point's restrictive answers. A guard can narrow what quack does, never widen it. With `fail: closed`, no decision restricts too. |
| `decide` | Uses it when confident, replacing the point's step; below `act_at` quack's own logic decides (`fallback`). |

A noul question's options are `true` and `false`; a score question's options are the level indexes `0`, `1`, ... The top answer is the most probable option, never a score's mean.

| Point | Modes | Questions | State |
| --- | --- | --- | --- |
| `plan.accept` | `observe` | `accept` (noul, primary): should a reviewer accept this plan step? | The user's request and, per node, its id, agent, task and dependencies, plus the plan's setup and its delivery kind (or `none`). A node an earlier step ran shows its status (`done`, `failed` or `cancelled`), its artifact kind and its output size in bytes, never the output itself; a node not yet run shows `pending`. The plan judge sees the earlier outputs; this state does not. It runs beside the plan judge and never changes the judge's verdict. |
| `memory.extract` | `observe` | `durable_fact` (noul, primary): does this turn state a durable fact about the user worth remembering? | The user's message and the user's previous message in the chat, if any (3,000 bytes each), asked when the turn ends; never the assistant's reply. Baseline: `true` when the user-memory hook's extraction produced a memory, `false` when it produced none or the keyword pre-filter skipped the turn, none when extraction failed. A pre-filtered turn is still asked and records `meta.prefiltered: true`. Asked only while `orchestrator.user_memory_hook` is enabled. |
| `answer.accept` | `observe` | `pass` (noul, primary): does this answer fully and correctly satisfy the task, with claims supported by the evidence shown? | The turn's user message as `request` (1,500 bytes), the node's planner-written assignment as `node_task` (2,000 bytes), its answer, and up to 750 bytes each of the URLs the answer cites and the URLs the node fetched. The answer gets what the handler's `max_input_tokens` leaves at two bytes per token, never under 6,000 bytes, and a cut keeps its head and its tail, where sources sit. Asked once per judge round; baseline: that round's pass or fail before a failed write-ahead-log save can force it closed, none when the judge call failed. |
| `research.source` | `observe` | `relevant` (noul, primary): is this page relevant to answering the research question? | Per page a `web-researcher` node fetched, once its answer is final: the turn's user message as `request` (1,500 bytes), the node's assignment as `node_task` (1,500 bytes), the page's first line as its title, its URL, and the stored text, budgeted and cut like `answer.accept`'s answer. Pages are asked one at a time across all nodes, so concurrent researchers add one request to the handler's queue, not one each; the first 20 per node are asked, in URL order; `meta` records the page's artifact id, its position, and how many pages were `skipped` over the cap. A page with no stored text is not asked. Baseline: whether the final answer cites the page's URL or artifact id. |

Every core state is clipped by bytes with a `…[truncated]` marker. The fixed caps fit a 4096-token handler; at that cap the 6,000-byte floor for an answer or page can still exceed it for dense text, which the server rejects and the point records as `unavailable`. All four core points run on their own goroutine and never delay the caller.

The [GitHub extension](../extensions/github.md) declares five points, none of which changes what it posts or runs. The three review points are asked after a review is posted: each finding's state is its path, line, text without its label, and the diff hunk it is anchored to; the verdict's state is the pull request's title and body, every finding (without severities), the changed files and the diff hunks, and never the reviewer's summary. `intent` is asked for a free-text mention on an issue or pull request, with the comment, the title, the sender and their author association, and the delivery kinds the labels and authorship grant. `ci.flaky` is asked per failing check (up to five) on the auto-heal and `quack:fix` paths, with the repo, pull request, head SHA, check name, the check's summary and annotations, and the first 50 changed files.

| Point | Modes | Questions | Restrictive | Baseline |
| --- | --- | --- | --- | --- |
| `ext:github/finding.severity` | `observe` | `severity` (choice, primary): `blocking`, `suggestion`, `nit`, `question` | `blocking` | The finding's severity from quack's review, else its body label; not asked for an unlabeled finding. |
| `ext:github/finding.blocking` | `observe` | `blocking` (noul, primary): would merging with this finding unaddressed be a mistake? | `true` | Whether the label is `blocking`. |
| `ext:github/review.verdict` | `observe` | `verdict` (choice, primary): `approve`, `comment`, `request_changes` | `comment`, `request_changes` | The review's own verdict. |
| `ext:github/intent` | `observe` | `write` (noul, primary): does the message ask quack to write to the repository? `deliverable` (choice): `reply`, `review`, `commit`, `pull_request`, `plan` | `false` | `true` when the extension's classifiers picked a commit or a new pull request; it covers `write` only. |
| `ext:github/ci.flaky` | `observe` | `flaky` (noul, primary): is this CI failure unrelated to the pull request's changes? | none | None at failure time; label it later by joining `(repo, head_sha, check)` to that check's re-run conclusion. |

The [Sleeper extension](https://github.com/fagerbergj/quack-extensions/tree/main/sleeper#decision-points) declares four points, asked one at a time after a `lineup`, `waivers` or `trade` job run ends `done`, from that job's artifact. They never change an artifact or a dispatch. Each state carries the player rows (id, name, position, team, opponent, injury and practice status, projection, floor and ceiling, recent points) and the analyst's reasoning for that row, without its verdict, `replaces`, confidence or rank, plus `chat_id`, `league_id`, `season` and `week` for joining against actual points.

| Point | Modes | Questions | Restrictive | Baseline |
| --- | --- | --- | --- | --- |
| `ext:sleeper/lineup_change` | `observe` | `swap` (noul, primary): should the proposed player start in this slot instead of the current one? | none | `true`, asked per recommended swap. |
| `ext:sleeper/waiver_pickup` | `observe` | `pickup` (noul, primary): is this add worth its drop and the waiver priority or FAAB? | none | `true` for a recommended add, `false` for one the scout checked and passed on. |
| `ext:sleeper/waiver_priority` | `observe` | `priority` (score, primary): five levels, fifth claim or later (`0`) to first claim (`4`) | none | `5 - min(rank, 5)`, asked per ranked add when two or more are ranked. |
| `ext:sleeper/trade_accept` | `observe` | `accept` (noul, primary): should this trade happen exactly as written? | none | `true` when the analyst's verdict is `send`, `false` for `decline` or `counter`. |

Points are built once at boot, so changing `decisions:` needs a restart.

## Recording

Every call an enabled point makes is recorded, including ones that returned no decision:

- **Ledger.** A `decision` observation entry on the run's chat, with the node and round when the point ran inside one. An extension that asks outside a run, as Sleeper does after a run ends, names the chat in the request's `ChatID`; quack accepts only the plugin's own `ext:<plugin>:` chats and rejects any other with a namespace error. It carries the point, mode, handler, outcome, top answer and its probability, and every question's probabilities. `confident` says whether the top answer reached `act_at`; it is recorded in every mode, so an observe run shows what guard or decide would have done. `baseline` is quack's own outcome for the same point, in the primary question's option space; it is absent when a `decide` outcome skipped the step that would have produced it. A `decide` point that replaces a step cannot record that step's real output on a `fallback`: the call is one-shot, so the caller's placeholder baseline is stored and the shadow comparison is not valid for that point. `skipped_step` names the step a `decide` outcome replaced and is null otherwise, so savings can be summed from the replaced step's recorded cost. The entry also carries request bytes, input tokens, server and client latency, any error, and the exact state and questions sent. `meta`, when a point sets it, is recorded beside the state but never sent to the handler, so a flag that correlates with the baseline cannot sway the answer; `quack decisions export --langfuse` puts it in the item metadata as `state_meta`. It is exported with the rest of a chat's observations by `quack ledger export`.
- **Trace.** A `quack.decision` span under the span that ran the point (for `plan.accept`, `quack.plan.judge`). It carries the same fields as `quack.decision.*` attributes, minus the state and questions.

Outcomes are `observe`, `pass`, `restrict`, `act`, `fallback`, and `unavailable`.

## Reading the data

`quack decisions report [--since 7d] [--point <id>] [--chat <id>...] [--json]` reads the ledger's `decision` entries through the server and prints, per point:

- **n, unavailable.** All recorded calls, and those that returned no answer.
- **coverage.** The share of answered calls whose top answer was `confident`.
- **agreement.** The share of answered calls with a baseline where the top answer equals `baseline`. Calls without a baseline (a skipped step) are left out.
- **confident disagreements.** Confident answers that differ from the baseline, listed with chat, node, both answers, and the top probability. A model that is wrong while confident is what `guard` and `decide` would act on.
- **latency, input tokens.** Mean and p50 client latency and p50 input tokens over answered calls.
- **confusion.** A table of the model's top answer against the baseline. A score question's options are its level indexes.

`quack decisions export --langfuse` writes the same entries to Langfuse for comparing strategies later. Each point becomes the dataset `decisions/<point id>`. An item's id is a hash of chat, node, round, point and the state sent, so re-exporting updates items in place. Its input is the state and questions, its expected output is `{baseline}`, and its metadata names the chat, node, round, handler, and mode. Each handler's answers go in a dataset run named `<handler>@<quack version>`: a trace holding the top answer, probability, all answers, outcome and `confident`, scored `agrees_with_baseline` (0 or 1) and `top_p`. Unavailable calls get a trace but no scores, and entries without a baseline are not exported. The version is the one the server is running now, not the one that recorded each entry, so export after upgrading only the window you want in the new run (`--since`). Credentials come from the local `quack.yaml`'s `prompts.store`; the command errors if it is not a langfuse store.
