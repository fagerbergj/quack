# Stores

`stores:` is a named backend registry, the same shape as `providers:` - consumers reference a store by name, and `kind` selects the adapter.

```yaml
stores:
  default_postgres:
    kind: postgres
    url: ${QUACK_DATABASE_URL}
  default_vector:
    kind: qdrant
    url: ${QUACK_QDRANT_URL}       # empty ⇒ memory self-disables
    embedder:
      provider: default
      model: ${QUACK_EMBED_MODEL}
    consolidation:
      provider: default
      model: ${QUACK_JUDGE_MODEL}
    top_k: 5
    min_score: 0.5
```

## Relational

Holds ADK sessions + events, the DAG plan and per-node state (what makes a run resumable after a restart), chat metadata, and structured memory. `kind: postgres` or `kind: sqlite` are both accepted for a relational store - `sqlite` is the no-container path.

`session.store` binds this store to ADK session/chat persistence:

```yaml
session:
  store: default_postgres
  schema: sessions   # reserved - ADK's session service exposes no schema param yet
```

## Vector

Backs semantic memory / RAG recall.
A vector store carries extra fields a relational store ignores:

- `embedder` - provider + model used to vectorize text.
- `consolidation` - provider + model for the ADD/UPDATE/DELETE/NOOP consolidation decision (quack reuses the judge model here, since it's already warm).
- `top_k` / `min_score` - recall defaults (neighbors fetched, minimum cosine similarity for a hit; `0` disables the floor). Overridable per tool.

An empty `url` (i.e. `QUACK_QDRANT_URL` unset) makes memory self-disable - a qdrant-less deployment keeps running, it just never recalls or commits memories.

## `extends`

A store can inherit another's fields, with its own fields overriding:

```yaml
stores:
  docs_vector:
    extends: default_vector
    collection: docs
```

This is how a second store reuses a connection under a different collection/schema without repeating the URL and embedder config.

## Referencing a store from a tool

A tool backed by shared infra (memory, RAG) references a store by name instead of declaring its own `kind`/`url`:

```yaml
tools:
  stage_memory:
    store: default_vector
    collection: task_memory
  commit_memory:
    store: default_vector
    collection: user_memory
```

The store supplies the adapter and connection; the tool may override `collection` / `schema` / `top_k` / `min_score` for its own namespace.
See [agents.md](agents.md) for which agents bind `stage_memory` (task memory) and the orchestrator's `commit_memory` (user memory).
