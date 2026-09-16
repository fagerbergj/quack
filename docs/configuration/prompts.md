# Prompts

Every system prompt, rubric, memory-guidance file, and lifted fragment quack sends to a model is a named artifact (`internal/artifactsrc`), sourceable from a Langfuse prompt-management project instead of the shipped file baked into the image. With no `prompts:` block, nothing changes: every artifact resolves to its shipped file, exactly as before this feature existed.

## What is sourceable

Each name mirrors the shipped file it falls back to. The registry (`artifactsrc.Names`) is derived from the shipped tree at boot, never hand-listed: every `agents/<x>/` bundle yields `system/<x>` plus `rubric/<x>` and `memory/<x>` when those files exist, and each `config/prompts/<n>.md` yields `system/<n>`.

| Name | Shipped file |
| --- | --- |
| `system/<agent>` | `agents/<agent>/prompt.md` |
| `rubric/<agent>` | `agents/<agent>/rubric.yaml` (only agents that have one) |
| `memory/<agent>` | `agents/<agent>/memory.md` (only agents that have one) |
| `rubric/global` | `config/rubric.md` |
| `rubric/constitution` | `config/constitution.md` |
| `system/judge` | `config/prompts/judge.md` |
| `system/compaction` | `config/prompts/compaction.md` |
| `system/compaction.summary` | `config/prompts/compaction.summary.md` |
| `system/acp.environment` | `config/prompts/acp.environment.md` |

`<agent>` is one of `agents/`'s bundle directories (`advisor`, `code-explorer`, `code-implementer`, `code-reviewer`, `image-reader`, `media-reader`, `memory-agent`, `orchestrator`, `synthesizer`, `web-researcher`). Only the five core names quack cannot run without - `system/judge`, `system/compaction`, `system/compaction.summary`, `system/acp.environment`, `rubric/global` - are checked at boot and log an error if missing (a broken image, or a bind-mount that shadowed the file); any other absent shipped file is silently absent from the registry instead.

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

Every name is resolved through a `Resolver`, which caches what it fetches from the store for `prompts.cache_ttl` (default 60s) before checking again - an edit made in Langfuse takes effect on the first resolve to run past that window, no restart required. `artifactsrc.Pinned` sits on top of the `Resolver` for the handful of names refreshed at a fixed point in a node's lifecycle (see "What refreshes when" below): it holds whatever it last resolved and only calls the `Resolver` again at that refresh point, so the prompt a round runs on and the version the ledger records for it can never disagree partway through.

Fallback to the shipped file happens on any of these:

- The store has no version for the name (`Get` returns not-found) - falls back **silently**, no warning logged.
- The store is unreachable or errors (network failure, 5xx, malformed response) - warns, rate-limited to once per name per TTL so a sustained outage doesn't spam the log on every resolve.
- The resolved body is blank (`ResolveUsable`) - someone emptied a prompt by accident, not an intentional edit, so the round runs on shipped instructions rather than none at all. Warns on **every** resolve until fixed, not rate-limited.
- The resolved body won't parse as a Go template, or (for `system/judge`) is missing a required template block (`artifactsrc.Render`) - a bad edit degrades to the shipped prompt rather than disabling the round it feeds. Also warns on every resolve until fixed.

An auth failure (401/403) is distinguished from an outage in the log line - it names the `stores.<name>` entry to check credentials on, rather than reading as a generic connectivity warning.

## Versions and pinning

Versions are Langfuse's own numbered prompt versions. The active version for a name is `latest` unless some version carries the pin label (`production` by default, `prompts.pin_label` to change it); labelling a different version moves the pin, and removing the label from all versions returns the name to `latest`.

## Seeding at boot

At boot, in the background (seeding never delays readiness or blocks a name's resolution), quack seeds every shipped artifact name into the store:

- **Not found** (`GET` 404): `POST` a new text prompt with the shipped body, empty `config` (so the static model/effort binding keeps applying until an operator sets one explicitly), tag `quack-seed`, commit message `quack-seed <static content hash>`. No labels are sent at all - Langfuse assigns `latest` itself, and the version never carries `pin_label` (`production` by default) even when one is configured.
- **Found, and the latest version's commit message doesn't start with `quack-seed`** (a person edited it, or authored the version directly - anything without the seed prefix, regardless of whether the shipped file itself changed): never touched. A warning logs once, naming the artifact, that the shipped prompt changed and the operator has edits to diff in the Langfuse UI. This warning fires on every boot as long as the latest version stays operator-authored, whether or not the shipped bytes have moved since.
- **Found, and the latest version is itself a stale seed** (commit message starts with `quack-seed` and the hash it carries differs from the current shipped hash): a new version is created the same way, with an updated commit message and the existing tags plus `quack-seed` carried forward (an operator's own tags on a prior seed are preserved, not overwritten) - so an untouched deployment's Langfuse prompts track the shipped files across an upgrade.
- **Found, and the latest seed's hash matches the current shipped hash**: nothing happens, silently.

A per-name seeding failure logs and never blocks boot or any other name.

## Composable references

Langfuse prompts support `@@@langfusePrompt:name=<name>|label=production@@@` (or `|version=N`) references, resolved server-side by Langfuse before the body reaches quack - a selector is required, and only text prompts can be referenced this way. This is the supported way to share a fragment (a house style note, a shared constraint) across multiple prompt names - nothing in quack builds or resolves these; they're pure Langfuse-side composition.

## Model, effort, and provider bindings

A resolved version can carry a `config` block with `model`, `provider`, and/or `effort` keys, overriding the round's static agent binding - see [models.md](models.md#binding-on-a-prompt) for the full validation rules (must be a registered model, provider must agree with the model's own, effort one of low/medium/high, an admission-limited model can't be swapped in) and the judge's own binding (`system/judge`'s `config` overrides `gates.judge.model`/`provider`, with `effort` landing on `thinking_level`). An invalid override is logged once and the round falls back to the static binding rather than failing.

## What refreshes when

Not every artifact is refreshed at the same point in a chat's lifecycle:

- **Per round** (pinned and refreshed at every worker/judge round's start): `system/<agent>`, `system/judge`, and `system/acp.environment` - the ACP environment block is rendered fresh on every round (`internal/acp/acp.go`'s `environmentBlock` call), not just resolved once per node.
- **Per run start** (the gate config refresh at the start of every run, `vetting.FromConfig` / `serve.go`'s `refreshGateCfg`): `rubric/global`, `rubric/constitution`, and `rubric/<agent>` (the last comes from `vetting.LoadBundleRubricSpecs`, not `FromConfig`, but refreshes at the same point) - an edited rubric reaches the next run without a restart, but not mid-run.
- **Per node dispatch** (`agent.Serve` building that node's compaction config via `NativeCompactionConfig`): `system/compaction`, `system/compaction.summary`.
- **Boot only, restart required**: `memory/<agent>` (`buildNativeNode`/`buildACPNode` read it once when the node is built; the ACP preamble cache keys only on the prompt version, so a memory-only edit never invalidates that cache either), `system/orchestrator` and `memory/orchestrator` (the orchestrator's system prompt is assembled once, into `orchSysPrompt`), and `system/advisor`/`system/memory-agent` (each is a `Pinned` that nothing ever calls `Refresh` on). These names are still seeded into and readable from Langfuse, but an edit to one of them needs a server restart to reach a running deployment.

This split also means only `system/<agent>` and `system/judge` carry per-call provenance (`prompt_source`/`prompt_version_id`/`prompt_artifact` on the `llm.call` ledger entry). Every other name above resolves live/static with no such provenance recorded - a known gap tracked in [#1439](https://github.com/fagerbergj/quack/issues/1439), not a deliberate design choice.

## Trace correlation and the delivered output

See [observability.md](observability.md#langfuse-prompts) for the exact span attribute keys a store-resolved round's generation spans carry, and how the delivered output lands on the trace for Langfuse-side scoring.

## Langfuse-side setup checklist

Nothing in quack builds these; they're configured directly in the Langfuse project quack's `prompts.store` points at.

1. **Register llm-swap as an LLM connection** in the Langfuse project's settings, so the playground and evaluators can call it - the same OpenAI-compatible endpoint `providers:` uses.
2. **Build an evaluator template from `rubric/global`** - the default rubric quack's own judge scores against (`rubric/constitution` is the separate, standing set of principles layered under every rubric), as the starting point for a Langfuse LLM-as-judge evaluator on the same criteria.
3. **Score the delivered output**: once a delivering node's delivery commits, its `quack.node` span is stamped with the delivered text as `langfuse.observation.output` and `langfuse.trace.output` (gated on `observability.otel.capture_content`) - the latter promotes it to the whole trace's output, so an evaluator or human annotation queue pointed at the trace scores what actually shipped, not any one round's draft. With more than one delivering node in a run, the last delivery to commit wins the trace-level attribute.
4. **Sessions**: every trace already carries `gen_ai.conversation.id` (the chat id) and `user.id` (the session's user, when known), so Langfuse's session view groups a chat's runs without extra wiring.
