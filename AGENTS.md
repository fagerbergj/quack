# AGENTS.md

This file provides guidance to AI coding agents working in this repository.

## Hard Rules

Never:

- Edit `internal/schema/quack.gen.go` or anything under `frontend/src/generated/` — these are generated; changes will be overwritten and CI will fail.
- Add an unrecognised file to an `agents/<name>/` bundle. Each bundle contains exactly `agent-card.json` and `prompt.md`, plus the optional `rubric.md` (judge rubric) and `memory.md` ("what to remember" guidance, M6).
- Edit `openapi.yaml` without running `make generate` and committing the regenerated files.

Always:

- Run `make generate` after any change to `openapi.yaml` and include the generated files in the same commit.
- Run `go test ./...` and `cd frontend && npm test` before marking a task done.
- Run `make vet && make fmt` before committing Go changes.

## Commands

```bash
# Build (compiles frontend first, embeds dist into binary)
make build

# Run Go tests
go test ./...

# Run a single package's tests
go test ./internal/vetting/...

# Go vet + fmt
make vet
make fmt          # gofmt -w internal cmd

# Frontend dev server (hot-reload at :5173)
cd frontend && npm run dev

# Frontend tests (vitest)
cd frontend && npm test

# Storybook
cd frontend && npm run storybook

# Type-check + lint frontend
cd frontend && npx tsc --noEmit && npx eslint src/

# Regenerate Go server stubs + TypeScript client from openapi.yaml
make generate     # runs scripts/generate.sh (oapi-codegen + openapi-ts)

# Lint openapi.yaml
npx @redocly/cli@latest lint openapi.yaml --config redocly.yaml

# Full stack (app + Postgres + searxng) via Docker
make docker-up
make docker-down
```

CI checks: `go vet`, `go test ./...`, `gofmt -l`, `tsc --noEmit`, `eslint`, `npm run build`, `npm test`, OpenAPI lint, and a codegen-drift check (`git diff --exit-code -- internal/schema frontend/src/generated`).

## Architecture

### Source of truth: `openapi.yaml`

`openapi.yaml` is the single source of truth for the API contract. `make generate` (via `scripts/generate.sh`) derives two artifacts from it:

- **`internal/schema/quack.gen.go`** — Go chi-server stubs + request/response types (oapi-codegen, config: `internal/schema/cfg.yaml`)
- **`frontend/src/generated/`** — TypeScript client (openapi-ts, config: `frontend/openapi-ts.config.ts`)

### Go module

Module path: `github.com/fagerbergj/quack`. The binary entrypoint is `cmd/server/main.go`.

### Request lifecycle

```
HTTP request
  → internal/server/router.go   (chi router; registers generated REST routes + MCP mount)
  → internal/server/rest/       (REST handler; dispatches to orchestrator)
  → internal/orchestrator/      (Orchestrator.Run: plan → execute → persist)
  → internal/dag/planner.go     (Planner.Plan: LLM call → DAG)
  → internal/dag/executor.go    (Executor.Execute: topological walk, layered concurrency)
  → each node → gated agent     (internal/vetting/gate.go wraps the worker)
  → internal/vetting/judge.go   (independent judge model scores output)
  → SSE events streamed back
```

### DAG execution (`internal/dag/`)

- `plan.go` — `Plan` and `Node` structs; `Node.DependsOn` encodes edges.
- `planner.go` — `Planner` makes a single LLM call to decompose a request into a `Plan`. The planner writes a per-node acceptance rubric alongside each node's task.
- `executor.go` — `Executor.Execute` topologically sorts the plan into layers, then runs each layer's nodes concurrently (bounded by a semaphore, default `maxActive=2`). Each node runs in a fresh ADK session (`plan.ID:node.ID`). Nodes downstream of a failed-gate node receive a warning prefix but execution continues (continue-but-warn policy).

### Trust gate (`internal/vetting/`)

Every node's output passes three cheapest-first stages before the DAG propagates it:

1. **Deterministic checks** (`gate.go`) — citation backing score + length check; targeted revise cycles up to `DeterministicRounds`.
2. **Self-refine** (`gate.go`) — the worker agent continues its own session to address its draft's gaps.
3. **Independent judge** (`judge.go`) — a separate agent with a different model calls `submit_verdict` (G-Eval style, 0–10 per criterion). Overall score = lowest criterion (weakest-link; no averaging). Score is normalised 0–1; threshold default `0.6`.

### Agent bundles (`internal/agent/`, `agents/`)

An agent bundle is a directory with exactly two required files (plus two optional ones):

```
agents/<name>/
  agent-card.json   # A2A AgentCard: identity + skills
  prompt.md         # system prompt
  rubric.md         # optional: per-agent judge rubric (falls back to config/rubric.md)
  memory.md         # optional: "what to remember" guidance (M6); appended to the
                    #   prompt only when the memory feature is on (agent.LoadBundleMemory).
                    #   Assumes the agent's tool list includes the memory tools
                    #   (load_memory / stage_memory) it refers to.
```

`agent.LoadBundle` reads the bundle; `agent.Build` turns it into an LLM agent. The config (`config/quack.yaml`) binds a model and tool list to each bundle. No code changes are needed to add or modify an agent.

### Inference (`internal/inference/`)

`inference.NewModel(providerConfig, modelName)` is the single factory for `model.LLM`. Only `kind: "openai"` is implemented (any OpenAI-compatible endpoint). Adding a new provider kind is localized to `factory.go`.

### Streaming (`internal/stream/`)

`stream.SSEEvent` is the wire-level vocabulary shared by REST, MCP, and A2A. The key event sequence within a node:

```
dag_plan → node_queued → node_start
  → agent_start (stage: worker)
  → agent_thinking / agent_tool_call / agent_tool_result / agent_token
  → agent_complete
  → [agent_start (stage: judge) … agent_complete]
  → [agent_start (stage: revise) … agent_complete]    (on judge fail, loops back to judge)
  → node_done (or node_failed)
done
```

`stream.Translator` converts raw ADK session events into this vocabulary. The `stage` field on `agent_start`/`agent_complete` (`worker`, `judge`, `revise`) lets the frontend group runs inside a node. A worker's `ask_advisor` consults (internal/tools/ask_advisor.go) are NOT a separate stage — they surface as ordinary `agent_tool_call`/`agent_tool_result` activity within the worker's own run.

### HTTP server (`internal/server/`)

- `router.go` — mounts MCP at `/api/v1/mcp`, registers generated chi routes, serves the embedded SPA for everything else.
- `rest/` — concrete HTTP handler that implements the generated `StrictServerInterface`.
- `mcp/` — MCP Streamable-HTTP server.
- The SPA (`frontend/dist`) is embedded into `cmd/server/web/dist` at build time (`make frontend-build`).

### Frontend (`frontend/`)

React 19 + Vite + Tailwind CSS 4 + TanStack Query. State is in `src/state/`:

- `chatStore.ts` — Zustand-like store for chat sessions and messages.
- `agentStream.ts` — parses the SSE event stream and updates the store.
- `ChatStoreProvider.tsx` — context provider.

Components under `src/components/` have co-located Storybook stories (`.stories.tsx`) and vitest tests (`.test.ts`). MSW (`msw`) mocks the API in tests and Storybook.

### Stores

- **Postgres** (GORM, `gorm.io/driver/postgres`) — ADK sessions + events, DAG plan/node state, chat metadata. Connection via env `QUACK_DATABASE_URL`.
- **qdrant** — semantic memory / RAG vectors. Connection via env `QUACK_QDRANT_URL`.

### Key env vars

| Var | Purpose |
|-----|---------|
| `LLM_BASE_URL` | OpenAI-compatible LLM endpoint (e.g. `http://jason-server:11436/v1`) |
| `QUACK_LLM_API_KEY` | API key |
| `QUACK_DATABASE_URL` | Postgres DSN |
| `QUACK_QDRANT_URL` | qdrant endpoint |
| `OIDC_ISSUER` / `OIDC_AUDIENCE` / `OIDC_JWKS_URL` | Inbound OIDC auth |
| `QUACK_LOG_LEVEL` | slog level: `debug`, `info` (default), `warn`, `error`. `debug` surfaces per-round hot-path trace (vetting/compaction). |
| `QUACK_LOG_FORMAT` | slog output: `text` (default, human-readable) or `json` (one object per line, for log aggregators). |

## Spec-Driven Development

Before implementing a non-trivial feature, write a brief spec covering:

- **Scope** — what is in and explicitly out of scope
- **Forbidden actions** — what the implementation must never do
- **Available tools / interfaces** — what it can call or depend on
- **Output contract** — shape and format of what it produces
- **Test cases** — at least 2–3 concrete input/output examples

Keep the spec in the PR description or a `docs/` file. Any behavioral drift from the spec becomes a failing test rather than a production incident.
