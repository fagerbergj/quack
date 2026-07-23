---
type: Architecture Overview
title: Quack System Architecture
description: High-level system architecture of Quack - monorepo layout, request lifecycle through the HTTP gateway to DAG execution and adversarial vetting, OpenAPI-driven code generation, native vs ACP agent types, SSE streaming vocabulary, model factory, and data stores (Postgres + qdrant).
tags: [architecture, overview, system-design]
resource: /README.md
---

# Quack System Architecture

Quack is a Go monorepo built on [Google ADK for Go][adk]. Clients submit requests through an HTTP gateway; an orchestrator decomposes each request into a **DAG** of agent nodes; each node runs in a generate→critique→revise→judge loop before its output propagates downstream or reaches the user.

[adk]: https://github.com/google-adk-go/adk-go "Google ADK for Go"

## Repository Layout

```
quack/
├── cmd/quack/main.go           # Binary entrypoint
├── config/                     # YAML configs (managed, quack settings)
├── agents/<name>/              # Agent bundles (agent-card.json + prompt.md [+ rubric.md, memory.md])
├── skills/                     # OpenWiki skill definitions (SKILL.md files)
├── frontend/src/               # React 19 SPA (Vite + Tailwind CSS 4)
│   ├── components/             # UI components with Storybook stories
│   ├── state/                  # Zustand-like chat store + SSE stream parser
│   └── generated/              # TypeScript client (auto-generated from openapi.yaml)
├── internal/                   # Go internals (see section below)
├── openapi.yaml                # Single source of truth for API contract
├── Makefile                    # Build, test, generate, docker targets
└── Dockerfile / docker-compose.yml  # Containerized deployment
```

Module path: `github.com/fagerbergj/quack`.

## Request Lifecycle

```
HTTP request (REST or MCP)
  → internal/server/router.go   (chi router; mounts REST routes + MCP + SPA)
  → internal/server/rest/handler.go  (implements generated StrictServerInterface)
  → internal/orchestrator/      (Orchestrator.Run: plan → DAG execution)
  → internal/dag/planner.go     (LLM decomposes request into Plan + Node rubrics)
  → internal/dag/nativegraph.go (topological ADK workflow graph execution)
  → each node → vetting.RunGatedRefine (internal/vetting/node.go - worker rounds + trust gate)
  → internal/stream/translator  (ADK session events → SSE vocabulary)
  → client receives streaming response
```

## API Contract and Code Generation

`openapi.yaml` is the single source of truth for Quack's HTTP API. The `make generate` target (`scripts/generate.sh`) derives two artifacts:

- **`internal/schema/quack.gen.go`** - Go chi-server stubs + request/response types via [oapi-codegen][oapi]
- **`frontend/src/generated/`** - TypeScript client via [@hey-api/openapi-ts][openapi-ts]

[oapi]: https://github.com/oapi-codegen/oapi-codegen "oapi-codegen"
[openapi-ts]: https://www.npmjs.com/package/@hey-api/openapi-ts "@hey-api/openapi-ts"

The API surface includes:

| Path | Method | Description |
|------|--------|-------------|
| `/health` | GET | Liveness check |
| `/api/v1/chats` | GET/POST | List chats / Create chat |
| `/api/v1/chats/{chat_id}` | GET/DELETE | Get/delete a chat with turns |
| `/api/v1/chats/{chat_id}/responses` | POST | Submit a response (triggers DAG execution) |
| `/api/v1/mcp` | - | Streamable-HTTP MCP server |

The API follows the [OpenResponses specification](https://openresponses.org/) - each chat turn is a Response containing typed OutputItems, including a custom `quack:dag` type for DAG state.

## Server Architecture

See also: [Workspace Isolation](/architecture/workspace-jail.md) · [Adversarial Trust Gate](/architecture/vetting.md)

[`internal/server/router.go`](/internal/server/router.go) wires three HTTP surfaces on a single chi router:

1. **REST API** - generated routes from `openapi.yaml` plus a `/health` endpoint
2. **MCP server** - Streamable-HTTP MCP at `/api/v1/mcp` (for Opencode, Claude Code integration)
3. **SPA serving** - embedded frontend dist; unmatched non-API routes fall through to the SPA for client-side routing

Extensions (`internal/extension/`) mount inbound routes (e.g. GitHub webhooks) as raw handlers outside the OpenAPI contract.

## Agent Types

See also: [DAG Execution](/workflows/dag-execution.md)

Quack agents fall into two categories:

### Native (llmagent) Bundles

Non-code agents run as native ADK LLM agents loaded from bundle directories:

```
agents/<name>/
  agent-card.json   # A2A AgentCard: identity + skills metadata
  prompt.md         # System prompt for the agent
  rubric.md         # Optional: per-agent judge rubric (falls back to config)
  memory.md         # Optional: "what to remember" guidance (native agents only)
```

Available native agents: **orchestrator**, **advisor**, **web-researcher**, **synthesizer**, **media-reader**, **image-reader**.

### External ACP Subprocesses

Code agents (**code-implementer**, **code-reviewer**, **code-explorer**) run as external subprocesses speaking the [Agent Client Protocol (ACP)](internal/acp/). They are spawned via `opencode acp` per worker round, with model binding via generated config and quack's skill library injected. They have no direct access to Quack tools - the gate's probes read their work off the clone or answer instead.

## Inference Layer

See also: [Adversarial Trust Gate](/architecture/vetting.md)

[`internal/inference/factory.go`](/internal/inference/factory.go) is the single factory for `model.LLM` instances. Currently only `kind: "openai"` is implemented (any OpenAI-compatible endpoint via the `adk-go-openai` adapter). Adding a new provider kind is localized to this file.

Every model built goes through `tracedModel` ([`internal/inference/traced.go`](/internal/inference/traced.go)) which records `quack.model.call.duration` for every inference call as an OTel metric.

Models are configured per-role in [`config/quack.yaml`](/config/quack.yaml): the orchestrator, researcher (worker), coder, and judge each name their own model and provider, with fallback to a shared default. The judge is deliberately independently-configured so its verdict comes from genuinely different weights.

## Streaming Vocabulary

See also: [DAG Execution](/workflows/dag-execution.md)

[`internal/stream/event.go`](/internal/stream/event.go) defines Quack's SSE event vocabulary, shared by REST and MCP transports. The model is flat and agent-centric: the DAG structure is separate from agent-run events within each node.

### DAG Structure Events

| Event | Purpose |
|-------|---------|
| `dag_plan` | Plan was generated |
| `node_queued` | Node entered execution queue |
| `node_start` | Node began execution |
| `node_done` | Node completed successfully |
| `node_failed` | Node failed |

### Agent Run Events (within a node)

| Event | Purpose |
|-------|---------|
| `agent_start` | Agent run begins (`stage: worker/judge/revise`) |
| `agent_thinking` | Reasoning text streamed during a run |
| `agent_tool_call` / `agent_tool_result` | Tool call and result pairs |
| `agent_token` | Output text (answer tokens) |
| `agent_complete` | Agent run ends with usage stats |

### Other Events

| Event | Purpose |
|-------|---------|
| `chat_title` | Chat title was generated |
| `error` | Error occurred |
| `done` | Full response complete |
| `delivery_result` | Staged item's delivery outcome (push/PR/review/comment) |

See also: [Workspace Isolation](/architecture/workspace-jail.md) · [DAG Execution](/workflows/dag-execution.md)

## Data Stores

- **Postgres** - ADK sessions and events, DAG plan/node state, chat metadata. Used via GORM (`gorm.io/driver/postgres`). Connection via `QUACK_DATABASE_URL`.
- **qdrant** - Semantic memory / RAG vectors. Connection via `QUACK_QDRANT_URL`. Memory only stores adversarially-vetted findings (via ADK's `MemoryService`: `AddSessionToMemory` to write, `SearchMemory` to recall).

## Recent Changes

- **Plan judge isolation** (`internal/tools/setup.go` - `SetupCloneReadOnly`, `internal/workspace/jail.go` - `PlanJudgeScope`): The plan judge now gets its own read-only repo clone in a reserved scope distinct from the DAG's shared repo, preventing race conditions during parallel execution.
- **Judgement fail-closed** (`internal/vetting/` - recent commits 5ef3a39): Budget-bounded judge prompt with deterministic fail-closed behavior on judge errors.
