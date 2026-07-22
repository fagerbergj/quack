# Models

## Providers

A provider is a named inference backend. `kind` picks the API protocol; the endpoint picks the actual server. Only `openai` is implemented today (any OpenAI-compatible endpoint) - `internal/inference.NewModel` is the single factory, so adding a new `kind` is localized to `internal/inference/factory.go`.

```yaml
providers:
  default:
    kind: openai
    endpoint: ${QUACK_LLM_ENDPOINT}
    api_key: ${QUACK_LLM_API_KEY}
```

Every agent references a provider by name (usually `default` - nothing stops a deployment from defining a second provider and pointing one agent at it).

## Per-agent inference

Each agent in `agents:` names its own `provider` + `model` + `context_window`:

```yaml
agents:
  web-researcher:
    provider: default
    model: ${QUACK_RESEARCHER_MODEL}
    context_window: 65536
```

There's no shared "default model" block - each agent's `model` is usually just an `${ENV}` reference, and the fallback lives in the env var itself: `code-implementer` and `code-reviewer` both read `${QUACK_CODER_MODEL}`, which falls back to `QUACK_RESEARCHER_MODEL` when unset (see [index.md](index.md#key-environment-variables)). That's the only chained fallback quack implements; every other agent's model is set directly or left unset (a startup error - `config.validate` rejects an agent with an empty model).

`context_window` isn't just documentation: it bounds automatic context compaction (`session.compaction`) and the judge's own prompt budgeting (`gates.judge.context_window`).

## The judge is a separate model

`gates.judge` (see [trust-gate.md](trust-gate.md)) names its own `provider` + `model`, independent of any worker's. That's deliberate - the trust gate's whole premise is that a genuinely different model catches blind spots a worker can't see in its own output. Reusing the worker's model for the judge would collapse that independence.

