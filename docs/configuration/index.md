# Configuration

Everything structural in quack — models, thresholds, budgets, stores, agent bindings — is declarative YAML, kept out of the code so it can change without a rebuild. Secrets never live in the file: any value that looks like a token, key, or DSN is written as an `${ENV_VAR}` reference and interpolated at load time (`internal/config.Load`). A `token`, `private_key`, or `webhook_secret` written as a literal instead of `${VAR}` is a hard startup error, not a silent leak.

The shipped config lives at `config/quack.yaml`; the schema and defaults are in `internal/config/config.go`.

## The `kind` shape

Providers, stores, and several tools are pluggable through a `kind` discriminator: `kind` picks the adapter, and the rest of the block is that adapter's connection details. Today only one implementation exists for most of these (`openai` for providers, `postgres`/`qdrant` for stores), but the shape leaves room to add another adapter later without touching every caller — see `internal/inference/factory.go` for providers and `internal/tools/backends.go` for tools.

## Layout

```yaml
providers:      # named inference backends (openai-compatible endpoints)
models:         # the canonical model registry (windows, admission limits, cost)
stores:         # named data backends (postgres, qdrant, sqlite)
session:        # ADK session/chat persistence + context compaction
orchestrator:   # the planner's model + tools + skills
agents:         # per-agent bundle bindings (model, tools, acp)
tools:          # built-in tool configuration
gates:          # the trust gate: deterministic checks + judge
dag:            # concurrency caps for the DAG executor
server:         # listen address, store topology, public_url
workspace:      # the agents' filesystem/git/run_command sandbox
skills:         # (deprecated alias for plugins) skill-library plugin roots
plugins:        # Agent Plugins roots (skills + mcp.json + quack extension declarations)
workflows:      # deployment-specific rows appended to the planner's workflow table
extensions:     # optional bundled integrations (e.g. GitHub App)
observability:  # otel tracing/metrics/logs emission + the ledger (WAL) store and observation toggle
auth:           # inbound OIDC bearer / trusted gateway headers (optional)
artifacts:      # the artifact store binding
```

Each section gets its own page below:

- **[Models](models.md)** — providers, per-agent inference, the llama-swap backend detail.
- **[Agents](agents.md)** — bundles, tool bindings, native vs. external ACP agents.
- **[Trust gate](trust-gate.md)** — deterministic checks, the independent judge, rubrics.
- **[Stores](stores.md)** — postgres, qdrant, and the named-store registry.
- **[Auth](auth.md)** — inbound OIDC and the gateway's trusted headers.
- **[Workspace](workspace/index.md)** — the filesystem jail, the OS sandbox, and the guard ladder.
  - **[Toolchains](workspace/toolchains.md)** — supplying Java/Android, Go, or any toolchain the image does not ship.
- **[Deployment shapes](deployment.md)** — three full worked examples ([`examples/`](examples/)): fully local, Docker stack, remote full-featured.
- **[Observability](observability.md)** — the OTel traces and metrics quack emits, and what each one is for.

The GitHub App extension (`extensions.github`) has its own page: [`../extensions/github.md`](../extensions/github.md).

## Context compaction (`session.compaction`)

Compaction runs on `google.golang.org/adk/v2`'s native runner-level engine (`internal/agent/a2a.go`'s `nativeCompactionConfig`), using quack's own summariser prompt (`internal/agent/compaction_prompts.go`) rather than adk's default. `provider`/`model` name an optional fallback summariser, used only when there's no active worker model to reuse.

- `token_threshold` — absolute safety limit, in tokens (adk uses the provider's reported prompt-token count, not an estimate). Unset ⇒ derived from the agent's `context_window`.
- `event_retention_size` — trailing request contents kept verbatim (default 20).
- `compaction_interval` — regular cadence, in invocations. `0` ⇒ threshold-only.
- `overlap_size` — how many of the newest folded-window events carry over raw into the next `compaction_interval` round instead of being folded in immediately (it does not affect a `token_threshold` round). `0` ⇒ fold the whole window each time. Requires `compaction_interval > 0` - adk rejects the combination otherwise.

adk's summariser hard-errors past its transcript cap (sized from `context_window`) rather than chunking, unlike a hand-rolled summariser would.

## `server`

- `addr` — listen address (default `:8080`).
- `topology` — `embedded` (sqlite, default), `managed` (brings up the docker-compose stores), or `external` (you run Postgres/Qdrant yourself).
- `shutdown_grace_seconds` — how long SIGTERM waits for in-flight runs before force-cancelling them (default 20).
- `public_url` — this server's externally reachable base URL, e.g. `https://quack.example.com`. Must be an absolute `http(s)` URL with no trailing slash when set. Passed to extensions (SDK `Host.PublicURL`) so a posted GitHub comment/review can link back to the run that made it; unset ⇒ no link. Interpolated from `QUACK_PUBLIC_URL`, which is optional — unset expands to empty, not a validation error.

## Key environment variables

| Var | Purpose |
|-----|---------|
| `QUACK_LLM_ENDPOINT` | OpenAI-compatible LLM endpoint (e.g. `http://jason-server:11436/v1`); interpolated into `providers.default.endpoint` |
| `QUACK_LLM_API_KEY` | API key |
| `QUACK_ORCH_MODEL` / `QUACK_RESEARCHER_MODEL` / `QUACK_CODER_MODEL` / `QUACK_JUDGE_MODEL` | Per-role model names (only `QUACK_CODER_MODEL` falls back, to `QUACK_RESEARCHER_MODEL`, if unset) |
| `QUACK_MEDIA_MODEL` / `QUACK_IMAGE_MODEL` | Media-reader / image-reader model names (no fallback; unset in the init wizard simply omits the agent) |

| `QUACK_EMBED_MODEL` | Embedding model for the vector store |
| `QUACK_COMPACTION_ENABLED` / `QUACK_COMPACTION_MODEL` | Toggle + model for history compaction |
| `QUACK_DATABASE_URL` | Postgres DSN |
| `QUACK_QDRANT_URL` | qdrant endpoint (unset ⇒ memory self-disables) |
| `QUACK_SEARXNG_URL` | SearXNG JSON API endpoint for web search |
| `QUACK_WORKSPACE_ROOT` | Filesystem sandbox root (default `./workspace`) |
| `QUACK_PUBLIC_URL` | This server's externally reachable base URL; interpolated into `server.public_url`. Optional — unset ⇒ no link in extension-posted comments/reviews |
| `QUACK_LOG_LEVEL` | slog level: `debug`, `info` (default), `warn`, `error` |
| `QUACK_LOG_FORMAT` | slog output: `text` (default) or `json` |
| `QUACK_CONFIG` | Path to `quack.yaml`, used when `--config` isn't passed |
| `QUACK_HOME` | CLI state dir (server registry, cached tokens); default `~/.quack` |

`QUACK_CODER_MODEL` is the one variable with a chained fallback: if it's unset, `code-implementer` and `code-reviewer` fall back to `QUACK_RESEARCHER_MODEL` (`internal/config`'s `expandEnv`), so a deployment that hasn't picked a dedicated coder model still gets a working one.
