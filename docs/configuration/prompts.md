# Prompts

Every system prompt, rubric, memory-guidance file, and lifted fragment quack sends to a model is a named artifact (`internal/artifactsrc`), sourceable from a Langfuse prompt-management project instead of the shipped file baked into the image. With no `prompts:` block, nothing changes: every artifact resolves to its shipped file, exactly as before this feature existed.

## What is sourceable

Each name mirrors the shipped file it falls back to. The registry (`artifactsrc.Names`) is derived from the shipped tree at boot, never hand-listed: every `agents/<x>/` bundle yields `system/<x>` plus `rubric/<x>` and `memory/<x>` when those files exist, and each `config/prompts/<n>.md` yields `system/<n>`.

| Name | Shipped file |
| --- | --- |
| `system/<agent>` | `agents/<agent>/prompt.md` |
| `rubric/<agent>` | `agents/<agent>/rubric.yaml` |
| `memory/<agent>` | `agents/<agent>/memory.md` (only agents that have one) |
| `rubric/global` | `config/rubric.md` |
| `rubric/constitution` | `config/constitution.md` |
| `system/judge` | `config/prompts/judge.md` |
| `system/compaction` | `config/prompts/compaction.md` |
| `system/compaction.summary` | `config/prompts/compaction.summary.md` |
| `system/acp.environment` | `config/prompts/acp.environment.md` |

`<agent>` is one of `agents/`'s bundle directories (`advisor`, `code-explorer`, `code-implementer`, `code-reviewer`, `image-reader`, `media-reader`, `memory-agent`, `orchestrator`, `synthesizer`, `web-researcher`). A shipped name that resolves to no file at boot (a broken image, or a bind-mount that shadowed it) logs an error rather than failing silently.

## The `prompts:` block and the langfuse store

Langfuse is a `stores:` entry like postgres and qdrant (`kind: langfuse`), and `prompts:` points at it by name:

```yaml
stores:
  langfuse:
    kind: langfuse
    url: ${LANGFUSE_BASE_URL}
    public_key: ${LANGFUSE_PUBLIC_KEY}
    secret_key: ${LANGFUSE_SECRET_KEY}

prompts:
  store: langfuse               # a stores[] entry of kind langfuse
  cache_ttl: 60s                # how long a resolved version is reused before re-checking
  pin_label: production         # a version carrying this label pins the name; otherwise latest wins
```

- `store` names the `stores:` entry; config validation rejects it if the entry doesn't exist or isn't `kind: langfuse`, and rejects a store with an empty `url`, `public_key`, or `secret_key` - a half-populated store would fall back to static on every name and look identical to Langfuse simply holding no prompts, so it's rejected at load instead.
- `cache_ttl` defaults to `60s` and must parse as a positive `time.Duration` when set.
- `pin_label` defaults to `production`.
- Omitting the whole `prompts:` block means static-only, today's behaviour, unconditionally: `stores:` may still declare a `kind: langfuse` entry (accepted by validation) with nothing pointing `prompts:` at it.

## Resolution semantics

A node round or judge round resolves its artifacts once, at the round's start, through a `Resolver` backed by a TTL cache (`artifactsrc.Pinned`). Nothing re-resolves mid-round: the prompt the model sees and the version the ledger records can never disagree partway through a round. The TTL (`prompts.cache_ttl`, default 60s) bounds how long a resolved artifact is reused across rounds before the resolver checks the store again - an edit made in Langfuse takes effect on the first round to start past that window, no restart required.

Fallback to the shipped file happens on any of these, each logged with a warning (rate-limited to once per name per TTL, so a sustained outage doesn't spam the log every round):

- The store has no version for the name (`Get` returns not-found).
- The store is unreachable or errors (network failure, 5xx, malformed response).
- The resolved body is blank (`ResolveUsable`) - someone emptied a prompt by accident, not an intentional edit, so the round runs on shipped instructions rather than none at all.
- The resolved body won't parse as a Go template, or (for `system/judge`) is missing a required template block (`artifactsrc.Render`) - a bad edit degrades to the shipped prompt rather than disabling the round it feeds.

An auth failure (401/403) is distinguished from an outage in the log line - it names the `stores.<name>` entry to check credentials on, rather than reading as a generic connectivity warning.

## Versions and pinning

Versions are Langfuse's own numbered prompt versions. The active version for a name is `latest` unless some version carries the pin label (`production` by default, `prompts.pin_label` to change it); labelling a different version moves the pin, and removing the label from all versions returns the name to `latest`.

## Seeding at boot

At boot, in the background (seeding never delays readiness or blocks a name's resolution), quack seeds every shipped artifact name into the store:

- **Not found** (`GET` 404): `POST` a new text prompt with the shipped body, empty `config` (so the static model/effort binding keeps applying until an operator sets one explicitly), tag `quack-seed`, commit message `quack-seed <static content hash>`, no `production` label.
- **Found, and the latest version is itself a stale seed** (its commit message starts with `quack-seed` and the hash it carries differs from the current shipped hash): a new version is created the same way, with an updated commit message - so an untouched deployment's Langfuse prompts track the shipped files across an upgrade.
- **Found, and the latest version's commit message doesn't start with `quack-seed`** (a person edited it, or authored it directly): never touched. A warning logs once, naming the artifact, that the shipped prompt changed and the operator has edits to diff in the Langfuse UI.
- **Found, and the latest seed's hash matches the current shipped hash**: nothing happens, silently.

A per-name seeding failure logs and never blocks boot or any other name.

## Composable references

Langfuse prompts support `@@@langfusePrompt:name=...@@@` references, resolved server-side by Langfuse before the body reaches quack. This is the supported way to share a fragment (a house style note, a shared constraint) across multiple prompt names - nothing in quack builds or resolves these; they're pure Langfuse-side composition.

## Model, effort, and provider bindings

A resolved version can carry a `config` block with `model`, `provider`, and/or `effort` keys, overriding the round's static agent binding - see [models.md](models.md#binding-on-a-prompt) for the full validation rules (must be a registered model, provider must agree with the model's own, effort one of low/medium/high, an admission-limited model can't be swapped in) and the judge's own binding (`system/judge`'s `config` overrides `gates.judge.model`/`provider`, with `effort` landing on `thinking_level`). An invalid override is logged once and the round falls back to the static binding rather than failing.

## What stays boot-captured

Not every artifact is refreshed per round. `rubric/global`, `rubric/constitution`, `rubric/<agent>` (via `vetting.FromConfig`), `memory/<agent>`, `system/compaction`, `system/compaction.summary`, and `system/acp.environment` are still resolved once, at bundle/node build time, rather than pinned-and-refreshed at each round's start the way `system/<agent>` and `system/judge` are. An edit to one of these still takes effect - the resolver itself has no restart requirement - but only on the next node build (a new run), not mid-run.

This split also means only `system/<agent>` and `system/judge` carry per-call provenance (`prompt_source`/`prompt_version_id`/`prompt_artifact` on the `llm.call` ledger entry). The other names above resolve live/static during replay, unpinned - a known gap tracked in [#1439](https://github.com/fagerbergj/quack/issues/1439), not a deliberate design choice.

## Trace correlation and the delivered output

See [observability.md](observability.md#langfuse-prompts) for the exact span attribute keys a store-resolved round's generation spans carry, and how the delivered output lands on the trace for Langfuse-side scoring.

## Replay pinning

See [observability.md](observability.md#ledger-and-recording) for how `quack replay` pins each `llm.call` to the exact prompt version it recorded, and the refusal messages for a version that no longer exists, a mismatched store, or a name that moved versions mid-run.

## Langfuse-side setup checklist

Nothing in quack builds these; they're configured directly in the Langfuse project quack's `prompts.store` points at.

1. **Register llm-swap as an LLM connection** in the Langfuse project's settings, so the playground and evaluators can call it - the same OpenAI-compatible endpoint `providers:` uses.
2. **Build an evaluator template from `rubric/global`** - the constitution and default rubric quack's own judge scores against, as the starting point for a Langfuse LLM-as-judge evaluator on the same criteria.
3. **Score the delivered output**: each run's root trace carries the delivered text as its output (`langfuse.observation.output`/`langfuse.trace.output`, gated on `observability.otel.capture_content`) - point an evaluator or a human annotation queue at that trace's output, not any one round's draft.
4. **Sessions**: every trace already carries `gen_ai.conversation.id` (the chat id) and `user.id` (the session's user, when known), so Langfuse's session view groups a chat's runs without extra wiring.
