# Quack — Configuration

The source of truth for Quack's volatile choices (models, endpoints, thresholds, budgets), kept
out of `PLAN.md` so they can change without touching the architecture. Secrets come from env.

| Config key | Value | Justification |
| --- | --- | --- |
| `providers.local.kind` | `openai` | API protocol the provider speaks (the `endpoint` picks the actual server). Other kinds (`gemini`, `anthropic`, …) possible in theory; only this one is implemented. |
| `providers.local.endpoint` (env `LLM_BASE_URL`) | `http://jason-server:11436/v1` | Local llama-swap endpoint; every agent shares it. |
| `providers.local.api_key` (env `LLM_API_KEY`) | `unused` | llama-swap needs no auth. |
| `orchestrator.planner.inference` | provider `local`, model `gpt-oss-120b` | Strongest reasoner; plans the DAG once per request, so quality beats speed. |
| `gates.deterministic_checks.max_rounds` | `4` | Free citation/length checks + cheap targeted revise cycles, run first. |
| `gates.self_critique.max_rounds` | `1` | Worker self-improvement passes before the judge; `0` disables. |
| `gates.judge.model` | `gemma4-26b-a4b` | Independent rubric scorer (empty ⇒ judge disabled); cheaper stages still run. |
| `gates.judge.threshold` | `0.6` | Per-criterion pass bar — every rubric criterion must clear it (verdict score = the lowest criterion; no averaging or hard caps). |
| `gates.judge.max_rounds` | `1` | Judge/revise rounds; bounds cost and keeps the node loop acyclic. |
| `agents[web-researcher].inference` | provider `local`, model `qwen3.6-35b` | Fast, capable general worker for web research. |
| `agents[web-researcher].tools` | `web_search, web_fetch, summarize, current_date, …` | Tool bindings (explicit; independent of the card's skills). |
| `agents[rag-researcher].inference` | provider `local`, model `qwen3.5-9b` | Smaller/faster worker; RAG lookup is lighter work. |
| `agents[rag-researcher].tools` | `rag_search` | Tool binding for the RAG researcher. |
| `tools.web_search.kind` | `searxng` (default) | Adapter for the dedicated search backend; empty ⇒ default. Swap with a new adapter + config, no tool rewrite (`internal/tools/backends.go`). |
| `tools.web_search.url` (env `SEARXNG_URL`) | _internal URL_ | Search backend endpoint (e.g. `http://searxng:8080`). |
| `tools.web_fetch.kind` | `crawl4ai` (default) | Adapter for the render backend; empty ⇒ default. |
| `tools.web_fetch.url` (env `CRAWL4AI_URL`) | _internal URL_ | Render backend for JS-heavy / bot-walled pages; empty ⇒ render fallback disabled. |
| `tools.<store-backed>.store` | a `stores[]` name | A tool backed by *shared* infra (e.g. memory) references a store instead of `kind`+`url`; the store supplies the adapter + connection. May override `collection` / `schema` / `top_k` / `min_score`. |
| `budget.max_nodes` | `12` | Per-request DAG size cap. |
| `budget.max_depth` | `4` | Per-request DAG depth cap. |
| `budget.max_tokens` | `400000` | Per-request token ceiling. |
| `budget.max_wall_clock` | `10m` | Per-request time ceiling. |
| `mcp[].name` / `.url` | e.g. `exa` / `https://mcp.exa.ai/mcp` | Outbound MCP server quack connects to as a client; its tools are discovered at runtime and handed to agents as a toolset (lazy — no startup network). The **no-docker web-search path**: Exa's hosted MCP is keyless (no API key, no container), serving `web_search_exa` + `web_fetch_exa`. |
| `mcp[].agents` / `.tools` | agent names / tool names | Optional: scope the toolset to specific agents (empty ⇒ all worker agents); allowlist which of the server's tools to expose (empty ⇒ all). |
| `stores.<name>.kind` | `postgres` / `qdrant` | **Named backend registry** (like `providers`): consumers reference a store by name. `kind` selects the adapter (the portability seam). Implemented: postgres, qdrant. |
| `stores.<name>.url` (env, e.g. `DATABASE_URL` / `QDRANT_URL`) | _secret_ | Connection endpoint. A vector store with an empty URL self-disables memory (qdrant-less runs keep working). |
| `stores.<name>.extends` | another store name | Inherit a store's fields (child overrides) — reuse one store's connection with a different schema/collection. |
| `stores.<vector>.embedder` / `.consolidation` | provider+model | Vector store only: how text is vectorized, and the ADD/UPDATE/DELETE/NOOP consolidation model (reuses the warm judge, gemma). |
| `stores.<vector>.top_k` / `.min_score` | `5` / `0.5` | Vector store recall defaults (neighbours fetched; min cosine for a hit, `0` disables). Overridable per tool. |
| `session.store` | a `stores[]` name (postgres) | ADK session + chat persistence backend. |
| `session.schema` | `sessions` | Reserved — ADK's session service exposes no schema param yet. |
| `session.compaction.*` (env `COMPACTION_ENABLED` / `COMPACTION_MODEL`) | disabled | Automatic context compaction over session history (prune old tool outputs, then summarise). |
| memory (no block) | — | Memory has **no config block — it's composed**. Task memory turns on when `stage_memory` binds a vector store (with `QDRANT_URL` set); user memory turns on when the **orchestrator** binds `commit_memory` (`orchestrator.tools: [commit_memory]`). The bound tool's store supplies embedder / consolidation / tuning. |
| `auth.oidc.issuer` (env `OIDC_ISSUER`) | Authentik OIDC issuer URL | IdP that issues/verifies tokens. Any OIDC IdP works (Keycloak, Auth0, …). |
| `auth.oidc.audience` (env `OIDC_AUDIENCE`) | `quack` | Expected token audience. |
| `auth.oidc.jwks_url` (env `OIDC_JWKS_URL`) | Authentik JWKS URL | Keys used to verify bearer tokens. |
| `auth.trusted_headers.user` | `X-authentik-username` | Identity header the gateway's forward-auth injects. |
| `auth.trusted_headers.groups` | `X-authentik-groups` | Groups header the gateway injects. |

Specialist agents are referenced as external [A2A AgentCard](https://a2a-protocol.org/latest/specification/)
JSON files (`agents[].card`), not inlined.

## Inference backend (llama-swap)

Models are served by a local [llama-swap](https://github.com/fagerbergj/home-server/tree/main/llm)
instance, OpenAI-compatible at `http://jason-server:11436/v1` (key `unused`). The worker
(`qwen3.6-35b`) and the judge (`gemma4-26b-a4b`) are **co-resident on the GPU** in llama-swap's
`chat` group (`swap:false`), so the judge never swaps the worker mid-request. The worker serves
`--parallel 2`, so up to **2 nodes run concurrently** (matched by `dag.max_active_nodes`). Other heavy
models (the coder, plus fallbacks) live in separate exclusive groups that swap the chat group in/out
on demand, and swapping those large models is expensive (multi-minute). See the home-server
`llm/llm-swap.yaml` for how each model is loaded.

## Gateway / deployment

Quack runs behind a [Traefik + Authentik gateway](https://github.com/fagerbergj/home-server/tree/main/api)
on the `api_gateway` network. Traefik routes `/api/v1/quack/*` to Quack with the `authentik@file`
forward-auth middleware — Authentik handles browser login and injects the `X-authentik-*` identity
headers Quack reads (see `auth.trusted_headers`); the public `openapi.yaml` route omits that
middleware. The OpenAPI spec is rendered by the gateway's central `swagger-ui` container — register
Quack by adding its spec URL to the `swagger-ui` `URLS` list in `api/docker-compose.yml`, the same
way `document-pipeline` is.
