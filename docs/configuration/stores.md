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
      retention_days: 30
      # forgetting:            # optional; see below - built-in defaults apply when absent
      #   rules:
      #     - when: 'tier == "unverified" && days_since_upvote > 90'
      #       then: invalidate
      #     - when: "score <= -2"
      #       then: invalidate
      #     - when: 'tier == "verified"'
      #       then: keep
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

Backs semantic memory / RAG recall. A vector store carries extra fields a relational store ignores:

- `embedder` - provider + model used to vectorize text.
- `consolidation` - provider + model for the ADD/UPDATE/DELETE/NOOP consolidation decision (quack reuses the judge model here, since it's already warm).
- `consolidation.retention_days` - hard-deletes invalidated memories (and their `memory_ops` rows) this many days after invalidation; `0` (default) keeps them forever. **Recommended: `30`** on a deployed server, so unverified memories the forgetting rules age out don't accumulate indefinitely.
- `consolidation.forgetting.rules` - ordered `{when, then}` age-out rules the nightly sweep evaluates, first match wins, no match keeps the memory (epic #1255 P3, full grammar in [memory-lifecycle.md](../memory-lifecycle.md#8c-epic-1255-p3-criteria-builder-age-out-retention)). **Optional** - omitting it entirely activates the built-in defaults (unverified + no upvote in 90 days -> invalidate; net score <= -2 -> invalidate; verified -> keep); no yaml edit is required to get P3 behavior. Run `quack memory sweep --dry-run` to see what the active rules would do before trusting them.
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

The store supplies the adapter and connection; the tool may override `collection` / `schema` / `top_k` / `min_score` for its own namespace. See [agents.md](agents.md) for which agents bind `stage_memory` (task memory) and the orchestrator's `commit_memory` (user memory). `recall_memory` (on-demand recall, epic #1255 P2) has no `tools:` entry of its own - it reads `stage_memory`'s store/collection/`top_k`/`min_score`, just on demand instead of at prefill time.
