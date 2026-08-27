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

`limits.active` caps how many DISTINCT models per role may be resident at once (issue #1007, enforced by `dag.Admission`); a role omitted from `active` is unbounded, and a provider without a `limits:` block accepts any number of resident models.

## Models registry

`models:` is the canonical model registry, a sibling of `providers:` and `agents:`, keyed by the RESOLVED model name (e.g. `QUACK_CODER_MODEL`'s actual value, not the env var) - that's the name `inference.NewModel` is called with:

```yaml
models:
  qwen3.8-27b:
    provider: default        # must name an entry under providers:
    role: worker             # matched against provider.limits.active's keys
    context_window: 262144   # the DEFAULT window handed to an agent that omits its own
    limits:
      sessions: 4             # max concurrent requests to this model (enforced by dag.Admission, #1007)
      kv_tokens: 262144        # admission pool; absent ⇒ context isn't a scheduling dimension
    cost:
      input_per_mtok: 0.60    # USD per million tokens, for gen_ai.client.cost / Langfuse
      output_per_mtok: 3.60
```

`limits` is entirely optional: absent means unlimited, never a conservative default. A model with no `limits:` gets arbitrary concurrent sessions at full context; a model with `limits` but no `kv_tokens` never participates in admission by context. Both `models.<m>.limits` and `providers.<p>.limits.active` are enforced by `dag.Admission` (#1007) on every gated node, and on the orchestrator's own turns (#1067) - so when `orchestrator.model` reuses a worker model, both draw on that model's one `sessions` pool. `cost` is optional too - a model without it gets token usage but no cost metric, never a guessed price.

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

## The orchestrator's own turns

The orchestrator draws on the same pool its worker nodes do, so `limits.sessions` caps how many turns generate at once. Its reservation is held only while a turn is actually generating: an orchestrator is idle while its DAG runs, and holding across that span would deadlock any config whose `sessions` cap is at or below the number of concurrent runs, since the orchestrators would own every session their own planned nodes wait on.

```yaml
orchestrator:
  model: ${QUACK_ORCH_MODEL}
  context_window: 65536     # optional; unset = its turns don't reserve kv at all
```

`orchestrator.context_window` is the kv reservation for a turn, the counterpart to an agent's own `context_window`. Unlike an agent it does **not** fall back to the model's window when unset - that would reserve the model's entire `kv_tokens` budget on every turn, blocking the very nodes the orchestrator just planned. Unset therefore means context is not a scheduling dimension for orchestrator turns. A declared window is validated against the model's `context_window` and `limits.kv_tokens`, like an agent's.

## How many runs may be live at once

`limits.sessions` bounds GPU work, not how many runs exist. A run holds a workspace clone from the moment it starts, and a live run is what the UI shows as a running chat - so tightening `sessions` alone reduces concurrent *generation* without reducing the number of chats showing as running.

```yaml
dag:
  max_active_runs: 2      # concurrent runs server-wide; default 8
  max_active_nodes: 32    # concurrent nodes WITHIN one run; default 32
```

`max_active_runs` is a host disk/CPU guard on run setup and the cap on how many runs are live, not a GPU knob - `limits.sessions` is that. Set both when you want few runs in flight *and* few of them generating.

## The judge is a separate model

`gates.judge` (see [trust-gate.md](trust-gate.md)) names its own `provider` + `model`, independent of any worker's. That's deliberate - the trust gate's whole premise is that a genuinely different model catches blind spots a worker can't see in its own output. Reusing the worker's model for the judge would collapse that independence.

