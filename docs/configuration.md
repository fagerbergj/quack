# Quack — Configuration

The source of truth for Quack's volatile choices (models, endpoints, thresholds, budgets), kept
out of `PLAN.md` so they can change without touching the architecture. Secrets come from env.

| Config key | Value | Justification |
| --- | --- | --- |
| `providers.local.kind` | `openai` | API protocol the provider speaks (the `endpoint` picks the actual server). Other kinds (`gemini`, `anthropic`, …) possible in theory; only this one is implemented. |
| `providers.local.endpoint` (env `LLM_BASE_URL`) | `http://jason-server:11436/v1` | Local llama-swap endpoint; every agent shares it. |
| `providers.local.api_key` (env `LLM_API_KEY`) | `unused` | llama-swap needs no auth. |
| `orchestrator.planner.inference` | provider `local`, model `gpt-oss-120b` | Strongest reasoner; plans the DAG once per request, so quality beats speed. |
| `adversarial.self_refine` | `true` | Free same-model pre-pass before the judge; polish, not the trust decision. |
| `adversarial.judge.inference` | provider `local`, model `selene-mini` | Atla Selene-1 Mini 8B, purpose-trained rubric scorer; independent of the worker, cheap to keep resident (see Inference backend). |
| `adversarial.judge.threshold` | `0.7` | Score bar to pass the gate. |
| `adversarial.max_rounds` | `2` | Revise-loop cap; bounds cost and keeps the node loop acyclic. |
| `agents[web-researcher].inference` | provider `local`, model `qwen3.6-35b` | Fast, capable general worker for web research. |
| `agents[web-researcher].tools` | `web_search, fetch, summarize` | Tool bindings (explicit; independent of the card's skills). |
| `agents[rag-researcher].inference` | provider `local`, model `qwen3.5-9b` | Smaller/faster worker; RAG lookup is lighter work. |
| `agents[rag-researcher].tools` | `rag_search` | Tool binding for the RAG researcher. |
| `tools.*.kind` | `builtin` | Registry of available tools (`web_search`, `fetch`, `summarize`, `rag_search`), all built-in for now; `mcp` / `http` reserved. |
| `budget.max_nodes` | `12` | Per-request DAG size cap. |
| `budget.max_depth` | `4` | Per-request DAG depth cap. |
| `budget.max_tokens` | `400000` | Per-request token ceiling. |
| `budget.max_wall_clock` | `10m` | Per-request time ceiling. |
| `stores.relational.kind` | `postgres` | Relational backend. `sqlite` etc. possible in theory; only Postgres implemented. |
| `stores.relational.url` (env `DATABASE_URL`) | _secret_ | DSN for the dedicated `quack` database. |
| `stores.vector.kind` | `qdrant` | Vector backend for long-term memory (M4). `pgvector` etc. possible in theory; only qdrant implemented. Empty disables memory. |
| `stores.vector.url` (env `QDRANT_URL`) | _secret_ | Qdrant endpoint. |
| `stores.vector.api_key` (env `QDRANT_API_KEY`) | _secret_ | Qdrant API key (optional; blank for keyless/local). |
| `stores.vector.collection` | `quack_memory` | Collection holding vetted findings. Created at startup, sized to the embedding model's dimension. |
| `embeddings.provider` | `default` | Named provider used for commit + recall vectors — the same llama-swap endpoint as the chat models. Empty disables memory. |
| `embeddings.model` (env `EMBEDDING_MODEL`) | `qwen3-embed` | Embedding model, selected by name on the shared endpoint. Changing it requires a fresh collection (dimension is fixed at creation). |
| `auth.oidc.issuer` (env `OIDC_ISSUER`) | Authentik OIDC issuer URL | IdP that issues/verifies tokens. Any OIDC IdP works (Keycloak, Auth0, …). |
| `auth.oidc.audience` (env `OIDC_AUDIENCE`) | `quack` | Expected token audience. |
| `auth.oidc.jwks_url` (env `OIDC_JWKS_URL`) | Authentik JWKS URL | Keys used to verify bearer tokens. |
| `auth.trusted_headers.user` | `X-authentik-username` | Identity header the gateway's forward-auth injects. |
| `auth.trusted_headers.groups` | `X-authentik-groups` | Groups header the gateway injects. |

Specialist agents are referenced as external [A2A AgentCard](https://a2a-protocol.org/latest/specification/)
JSON files (`agents[].card`), not inlined.

## Inference backend (llama-swap)

Models are served by a local [llama-swap](https://github.com/fagerbergj/home-server/tree/main/llm)
instance, OpenAI-compatible at `http://jason-server:11436/v1` (key `unused`). It holds **one chat
model in memory at a time** (the `main` group), and swapping a model is expensive (multi-minute for
the large ones), which is why Quack's executor runs nodes sequentially. The embedding model and the
CPU judge (`selene-mini`) are loaded **separately and stay resident**, so they never swap the GPU
chat model. See the home-server `llm/llm-swap.yaml` for how each model is loaded.

## Long-term memory (M4)

Memory is **optional and all-or-nothing**: set both `stores.vector` and `embeddings`, or neither
(a half-configured memory is rejected at startup). When enabled, the app ensures the Qdrant
collection exists at startup — sizing it to the embedding model's vector dimension, which it
discovers with a probe embedding — and fails fast if an existing collection has a different size
(changing the embedding model means a fresh collection, not a destructive auto-migration).

- **Scope** is the ADK default: each memory is keyed by `(appName, userID)`, and every agent serves
  under its own app name, so an agent recalls only **its own** prior vetted findings.
- **Commit** is gated by the trust gate: only an answer that **passes the judge** is stored. The
  point ID is derived from `(appName, userID, query)`, so re-asking the same question or a judge
  revise-loop **upserts in place** rather than duplicating.
- **Recall** is wired per-agent via two tools selected in the agent's `tools:` list: `load_memory`
  (LLM-invoked, surfaces as a visible `tool_call`) and `preload_memory` (auto-injects relevant
  findings into the prompt). Both are stripped automatically when memory is disabled.
- The embedding model (`qwen3-embed`) is **resident** in llama-swap's persistent group and reached
  through the same endpoint as the chat models (`LLM_ENDPOINT`), selected by name — so commit and
  recall never swap the GPU chat model. (It cold-loads on first use; the startup probe retries.)

A commit surfaces as a `memory_commit` SSE event; recall surfaces as the `load_memory` tool's
`tool_call` / `tool_result`.

## Gateway / deployment

Quack runs behind a [Traefik + Authentik gateway](https://github.com/fagerbergj/home-server/tree/main/api)
on the `api_gateway` network. Traefik routes `/api/v1/quack/*` to Quack with the `authentik@file`
forward-auth middleware — Authentik handles browser login and injects the `X-authentik-*` identity
headers Quack reads (see `auth.trusted_headers`); the public `openapi.yaml` route omits that
middleware. The OpenAPI spec is rendered by the gateway's central `swagger-ui` container — register
Quack by adding its spec URL to the `swagger-ui` `URLS` list in `api/docker-compose.yml`, the same
way `document-pipeline` is.
