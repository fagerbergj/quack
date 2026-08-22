# Models

## Providers

A provider is a named inference backend. `kind` picks the API protocol; the endpoint picks the actual server. Only `openai` is implemented today (any OpenAI-compatible endpoint) - `internal/inference.NewModel` is the single factory, so adding a new `kind` is localized to `internal/inference/factory.go`.

```yaml
providers:
  default:
    kind: openai
    endpoint: ${QUACK_LLM_ENDPOINT}
    api_key: ${QUACK_LLM_API_KEY}
    limits:
      active:          # how many DISTINCT models per role may be resident at once
        worker: 1
        judge: 1
        embed: 1
```

`limits.active` is parsed and validated but not yet enforced - it's the config-layer prerequisite for issue #1007 (capacity-based admission); a role omitted from `active` is unbounded, and a provider without a `limits:` block accepts any number of resident models.

## Models registry

`models:` is the canonical model registry, a sibling of `providers:` and `agents:`, keyed by the RESOLVED model name (e.g. `QUACK_CODER_MODEL`'s actual value, not the env var) - that's the name `inference.NewModel` is called with:

```yaml
models:
  qwen3.8-27b:
    provider: default        # must name an entry under providers:
    role: worker             # matched against provider.limits.active's keys
    context_window: 262144   # the DEFAULT window handed to an agent that omits its own
    limits:
      sessions: 4             # max concurrent requests to this model (#1007, inert)
      kv_tokens: 262144        # admission pool; absent ⇒ context isn't a scheduling dimension
    cost:
      input_per_mtok: 0.60    # USD per million tokens, for gen_ai.client.cost / Langfuse
      output_per_mtok: 3.60
```

`limits` is entirely optional and, like `providers.<p>.limits`, inert until #1007: absent means unlimited, never a conservative default. A model with no `limits:` gets arbitrary concurrent sessions at full context; a model with `limits` but no `kv_tokens` never participates in admission by context. `cost` is optional too - a model without it gets token usage but no cost metric, never a guessed price.

## Per-agent inference

Each agent in `agents:` names a `model` (a key into the registry above) and, optionally, its own `context_window`:

```yaml
agents:
  web-researcher:
    model: ${QUACK_RESEARCHER_MODEL}
    context_window: 65536
```

`provider:` on an agent is optional - it's derived from the model's registry entry. Set it explicitly only to catch drift: if it disagrees with the model's own provider, that's a config error.

There's no shared "default model" block - each agent's `model` is usually just an `${ENV}` reference, and the fallback lives in the env var itself: `code-implementer` and `code-reviewer` both read `${QUACK_CODER_MODEL}`, which falls back to `QUACK_RESEARCHER_MODEL` when unset (see [index.md](index.md#key-environment-variables)). That's the only chained fallback quack implements; every other agent's model is set directly or left unset (a startup error - `config.validate` rejects an agent with an empty model).

`context_window` bounds automatic context compaction (`session.compaction`) and the judge's own prompt budgeting (`gates.judge.context_window`); left unset, it defaults to the model's own `context_window`. It can never exceed the model's `context_window`, and if the model declares `limits.kv_tokens`, it can never exceed that either - a window that could never fit in the model's admission pool is a config error, not a runtime deadlock.

## The judge is a separate model

`gates.judge` (see [trust-gate.md](trust-gate.md)) names its own `provider` + `model`, independent of any worker's. That's deliberate - the trust gate's whole premise is that a genuinely different model catches blind spots a worker can't see in its own output. Reusing the worker's model for the judge would collapse that independence.

