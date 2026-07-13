# Quack — Configuration

The source of truth for Quack's volatile choices (models, endpoints, thresholds, budgets), kept
out of `PLAN.md` so they can change without touching the architecture. Secrets come from env.

| Config key | Value | Justification |
| --- | --- | --- |
| `providers.local.kind` | `openai` | API protocol the provider speaks (the `endpoint` picks the actual server). Other kinds (`gemini`, `anthropic`, …) possible in theory; only this one is implemented. |
| `providers.local.endpoint` (env `LLM_BASE_URL`) | `http://jason-server:11436/v1` | Local llama-swap endpoint; every agent shares it. |
| `providers.local.api_key` (env `QUACK_LLM_API_KEY`) | `unused` | llama-swap needs no auth. |
| `orchestrator.planner.inference` | provider `local`, model `gpt-oss-120b` | Strongest reasoner; plans the DAG once per request, so quality beats speed. |
| `gates.deterministic_checks.max_rounds` | `4` | Free citation/length checks + cheap targeted revise cycles, run first. |
| `gates.judge.model` | `gemma4-26b-a4b` | Independent rubric scorer (empty ⇒ judge disabled, and the ask_advisor tool with it); cheaper stages still run. |
| `gates.judge.threshold` | `0.6` | Per-criterion pass bar — every rubric criterion must clear it (verdict score = the lowest criterion; no averaging or hard caps). |
| `gates.judge.max_rounds` | `1` | Judge/revise rounds; bounds cost and keeps the node loop acyclic. |
| `agents[web-researcher].inference` | provider `local`, model `qwen3.6-35b` | Fast, capable general worker for web research. |
| `agents[web-researcher].tools` | `web_search, web_fetch, summarize, current_date, …, git_clone, read_file, list_dir, glob, grep` | Tool bindings (explicit; independent of the card's skills). The read-only fs + clone tools let a research task pull a repository apart; no write/edit/commit for the researcher — that's the code-implementer's job. |
| `agents[code-implementer].inference` | provider `local`, model `${QUACK_CODER_MODEL}` (falls back to `${QUACK_RESEARCHER_MODEL}` if unset — `internal/config`'s `expandEnv`) | Clones/edits/commits real code. Bundle `agents/code-implementer` (`prompt.md` = the ponytail ladder; `rubric.md` = the code-quality research criteria + a first-class ponytail section). |
| `agents[code-implementer].tools` | `read_file, write_file, edit_file, list_dir, glob, grep, delete_path, git_clone, git_checkout, git_status, git_diff, git_log, git_commit, git_branch, git_worktree_create, git_worktree_remove, git_push, git_pull, git_rebase, run_command, ask_advisor, ask_user, load_memory, stage_memory` | The full fs + git surface, plus `run_command` for its own build/test iteration loop (guarded — see `workspace.guards`), and shared memory (`load_memory`/`stage_memory` + the bundle's `memory.md`) so it recalls what the explorer/reviewer already learned about this repo. |
| `agents[<name>].memory_role` | `coding` / `research` (empty ⇒ none) | The agent's role bucket in **shared memory**. Memory is bucketed by SUBJECT — the repo, the role family, the user — not by agent (`internal/memory/scope.go`), so the explorer's repo knowledge reaches the implementer and the reviewer. |
| `agents[rag-researcher].inference` | provider `local`, model `qwen3.5-9b` | Smaller/faster worker; RAG lookup is lighter work. |
| `agents[rag-researcher].tools` | `rag_search` | Tool binding for the RAG researcher. |
| `tools.web_search.kind` | `searxng` (default) / `exa` | Search backend. `searxng` needs an instance URL. `exa` works **keyless** by default — it speaks Exa's hosted MCP under the hood, so no URL and no container (the no-docker path); add `auth` to use Exa's REST API instead (structured JSON, more robust). Either way the agent just calls `web_search`; swapping the backend is a config change, not a tool rewrite (`internal/tools/backends.go`). |
| `tools.web_search.url` (env `QUACK_SEARXNG_URL`) | _internal URL_ | SearXNG endpoint (e.g. `http://searxng:8080`); **required** for `kind: searxng`, unused by `kind: exa`. |
| `tools.<tool>.auth.kind` / `.api_key` | `api_key` / _secret_ | Optional backend auth. `kind: api_key` supplies an API key (e.g. `web_search` `kind: exa` → Exa REST). `oauth` is planned. Without `auth`, `exa` uses its keyless MCP surface. |
| `tools.web_fetch.kind` | `direct` (default) / `crawl4ai` | Fetch implementation. `direct` = a plain SSRF-guarded GET, no external service (the no-docker path). `crawl4ai` = the same GET plus a headless-browser render fallback for JS-heavy / bot-walled pages (needs a URL). Empty ⇒ `direct`. |
| `tools.web_fetch.url` (env `QUACK_CRAWL4AI_URL`) | _internal URL_ | crawl4ai render backend (e.g. `http://crawl4ai:11235`); **required** for `kind: crawl4ai`, unused by `kind: direct`. |
| `tools.<store-backed>.store` | a `stores[]` name | A tool backed by *shared* infra (e.g. memory) references a store instead of `kind`+`url`; the store supplies the adapter + connection. May override `collection` / `schema` / `top_k` / `min_score`. |
| `budget.max_nodes` | `12` | Per-request DAG size cap. |
| `budget.max_depth` | `4` | Per-request DAG depth cap. |
| `budget.max_tokens` | `400000` | Per-request token ceiling. |
| `budget.max_wall_clock` | `10m` | Per-request time ceiling. |
| `stores.<name>.kind` | `postgres` / `qdrant` / `sqlite` | **Named backend registry** (like `providers`): consumers reference a store by name. `kind` selects the adapter (the portability seam). sqlite backs both the session store and the memory vector index (pure-Go, no container — the no-docker path). |
| `stores.<name>.url` (env, e.g. `QUACK_DATABASE_URL` / `QUACK_QDRANT_URL`) | _secret_ | Connection endpoint. A vector store with an empty URL self-disables memory (qdrant-less runs keep working). |
| `stores.<name>.extends` | another store name | Inherit a store's fields (child overrides) — reuse one store's connection with a different schema/collection. |
| `stores.<vector>.embedder` / `.consolidation` | provider+model | Vector store only: how text is vectorized, and the ADD/UPDATE/DELETE/NOOP consolidation model (reuses the warm judge, gemma). |
| `stores.<vector>.top_k` / `.min_score` | `5` / `0.5` | Vector store recall defaults (neighbours fetched; min cosine for a hit, `0` disables). Overridable per tool. |
| `session.store` | a `stores[]` name (postgres or sqlite) | ADK session + chat persistence backend. |
| `session.schema` | `sessions` | Reserved — ADK's session service exposes no schema param yet. |
| `server.addr` | `:8080` | HTTP listen address. Override per-invocation with `quack server run --port <n>`. |
| `server.topology` | `external` (default) / `embedded` / `managed` | How the stores are provided. `embedded` (sqlite) and `external` (you run Postgres/Qdrant) just run the process. `managed` makes `quack server` bring up Postgres + Qdrant via an embedded `docker compose` stack (`quack-stores`), wait for readiness, then run; `quack server stop` tears the stack down (named volumes persist). `managed` needs docker + host ports 5432 and 6334 free. Tool backends (search/fetch) are **not** managed — configure them via `tools.*`. |
| `session.compaction.*` (env `QUACK_COMPACTION_ENABLED` / `QUACK_COMPACTION_MODEL`) | disabled | Automatic context compaction over session history (summarise older turns into an anchored summary, then drop them). |
| memory (no block) | — | Memory has **no config block — it's composed**. Task memory turns on when `stage_memory` binds a vector store (with `QUACK_QDRANT_URL` set); user memory turns on when the **orchestrator** binds `commit_memory` (`orchestrator.tools: [commit_memory]`). The bound tool's store supplies embedder / consolidation / tuning. Memories are **shared and bucketed by subject** — `repo:<repo>` (every coding agent working in that repo), `role:coding` / `role:research` (`agents[<name>].memory_role`), `user:<id>` — so what one agent learns, the next one recalls. |
| `workspace.root` (env `QUACK_WORKSPACE_ROOT`) | `./workspace` (compose: `/workspace`, a named volume) | The agents' working disk for the filesystem/git/run_command tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `delete_path`, `git_clone`, `git_checkout`, `git_status`, `git_diff`, `git_log`, `git_commit`, `git_branch`, `git_push`, `git_worktree_create`, `git_worktree_remove`, `git_pull`, `git_rebase`, `run_command`, …). One configured root; every operation resolves inside a per-user jail under it (`<root>/<user_id>/` — `user_id` is `"local"` today, the OIDC subject later) via the ONE path-resolution function in `internal/workspace` — `..` escapes, absolute paths, and symlinks pointing outside the jail are all rejected with a uniform error. Every `run_command`/`checks`/git child process also gets its own `$HOME` (`<root>/<user_id>/.quack-home`, created automatically) — a SIBLING of any cloned repo, never the repo's own working directory, so a toolchain's cache/config writes (`npm`'s `_cacache`, `~/.gitconfig`) never land inside a git working tree where a later `git_commit`'s blind `add_all` could sweep them up (see `workspace.check_commands`' row and `git_commit`'s own description for the file-count sanity wall that guards the rest of that path). |
| `workspace.max_read_kb` / `.max_write_kb` | `256` / `2048` | Per-call byte caps for `read_file` / `write_file`. An oversized `read_file` truncates and sets `truncated: true` (never an error); an oversized `write_file` errors (its result carries no `truncated` field to signal a silent partial write). |
| `workspace.max_results` / `.max_list_entries` | `200` / `500` | Per-call result caps for `grep`/`glob` and `list_dir` respectively — capped results set `truncated: true`, never an error. |
| `workspace.timeout_seconds` | `60` | Per-invocation timeout for git commands, `run_command`, and orchestrator-set deterministic checks — the latter two execute through the ONE jailed runner (`internal/workspace.RunPipeline`; a multi-stage pipeline shares a single overall deadline). |
| `workspace.check_commands` | `[]` (checks unavailable; e.g. `["go build", "go test", "go vet", "npx tsc", "npm test"]` when the deployment's image carries the toolchain) | Allowed command **prefixes** the planner may complete into a code-implementer node's per-node `checks` (§4 of `.quack/plan-pr5-tool-schemas.md`). The planner rejects a plan whose `checks` don't prefix-match one of these (or contain a shell metacharacter — pipes are allowed and run natively; `& ; $ < > \` ( )` stay unexpressible) at PLAN time, not run time; when this list is empty the planner is told checks are unavailable and omits them. The trust gate's deterministic stage then runs each configured check via the shared jailed pipeline runner (`workspace.RunPipeline`) in the node's `workdir` after every draft, folding a failure into criterion `checks_pass = 0` (weakest-link, like `grounded_in_retrieval`) — command + output tail become part of the revise prompt's feedback. |
| `workspace.git_credentials` | `[]` (public repos only) | Deployment-level per-host HTTPS git credentials — one identity per host (`host`, `username` default `x-access-token`, `token`). Matching is by exact host; no credential ⇒ the operation proceeds unauthenticated. `token` **must** be an `${VAR}` env reference in the raw YAML — a literal secret pasted here is a startup error (checked on the raw file text, before `${VAR}` expansion), never a silent leak. Never put credentials in a clone URL — `git_clone` rejects them outright. |
| `workspace.git_push` | `false` | Gates the `git_push` tool — the one outward-facing, non-undoable git operation in the set. Even when `true`, `git_push` never force-pushes (unexpressible — no argv path ever adds `--force`), rejects pushes to `main`/`master`, and requires a configured credential for the remote's host. |
| `workspace.guards` | shipped defaults below | Tool name → guard tier: `none` (default for anything unlisted) \| `judge` \| `confirm` \| `judge+confirm`. Tier 0 (the jail, argv-only exec, no shell, no force-push/push-to-main) always applies regardless of tier and is never bypassed by a guard result. `judge` runs an independent safety-judge model call (reusing `gates.judge`'s provider/model, an isolated single-shot run) before the tool executes; a denial returns the refusal as the tool's result and the tool never runs. `confirm` pauses the DAG node for a human approve/deny, riding the exact same mid-node pause/resume path as a worker's `ask_user` question (reply "approve" or "deny"). Shipped defaults: `delete_path: judge`, `git_rebase: judge`, `git_push: judge+confirm`, `run_command: judge+confirm` (honest limit: the jail confines the working directory, not what the program itself does — the real containment boundary is the deployment's own container). Override freely — a cautious deployment can `confirm` everything; a trusting one can drop tools to `none`. |
| `auth.oidc.issuer` (env `OIDC_ISSUER`) | Authentik OIDC issuer URL | IdP that issues/verifies tokens. Any OIDC IdP works (Keycloak, Auth0, …). |
| `auth.oidc.audience` (env `OIDC_AUDIENCE`) | `quack` | Expected token audience. |
| `auth.oidc.jwks_url` (env `OIDC_JWKS_URL`) | Authentik JWKS URL | Keys used to verify bearer tokens. |
| `auth.trusted_headers.user` | `X-authentik-username` | Identity header the gateway's forward-auth injects. |
| `auth.trusted_headers.groups` | `X-authentik-groups` | Groups header the gateway injects. |
| `extensions.github` | absent (off) | The GitHub App extension — inbound webhook (`/api/v1/github/webhook`) + outbound tools (`github_comment`, `github_pull_request`) + git auth via the App installation token. Keys: exactly one issuer — `client_id` (recommended) or `app_id` — plus one of `private_key` (`${VAR}` PEM) / `private_key_path`, `webhook_secret` (`${VAR}`) and `mention` (default `@quack`). The client secret is not used (auth signs a JWT with the private key). Secrets must be `${VAR}` references (a literal is a startup error). Full setup + the non-interactive guard policy: [`docs/github-app.md`](github-app.md). |

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
