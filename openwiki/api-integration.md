---
type: "Reference"
title: "API and Integrations"
openwiki_generated: true
---

# API and Integrations

Quack provides multiple interfaces: REST HTTP API, MCP, A2A, and webhook extensions.

## REST API

The OpenAPI spec (`openapi.yaml`) defines the REST contract:

### Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET /health` | Liveness check |
| `POST /api/v1/chats` | Create a new chat |
| `GET /api/v1/chats` | List all chats |
| `GET /api/v1/chats/{chat_id}` | Get chat details |
| `DELETE /api/v1/chats/{chat_id}` | Delete a chat |
| `POST /api/v1/chats/{chat_id}/responses` | Send message (streaming) |
| `GET /api/v1/chats/{chat_id}/responses/{response_id}` | Get a specific response |
| `GET /api/v1/chats/{chat_id}/stream` | Subscribe to SSE stream |

### SSE Event Vocabulary

The `/responses` endpoint streams Server-Sent Events with these event types:

| Event | Payload | Purpose |
|-------|---------|---------|
| `response_created` | `{response_id}` | First event, names the turn |
| `agent_start` | `{node_id,run_id,agent,stage,round}` | Opens a worker/judge/revise run |
| `agent_thinking` | `{node_id,run_id,text}` | Model reasoning |
| `agent_tool_call` | `{node_id,run_id,call_id,name,args}` | Tool invocation |
| `agent_tool_result` | `{node_id,run_id,call_id,name,result}` | Tool result |
| `agent_token` | `{node_id,run_id,text}` | Answer token streaming |
| `agent_complete` | `{node_id,run_id,stage,score,passed,feedback}` | Closes a run |
| `dag_plan` | `{plan_id,nodes,edges}` | DAG structure added |
| `node_queued` | `{node_id}` | Node scheduled |
| `node_start` | `{node_id,agent}` | Node execution began |
| `node_done` | `{node_id,score,passed,feedback,...}` | Node completed |
| `node_failed` | `{node_id,error}` | Node failed |
| `node_cancelled` | `{node_id}` | Node cancelled |
| `node_needs_input` | `{node_id}` | HITL pause requested |
| `node_steered` | `{node_id,steer}` | User steering guidance |
| `delivery_result` | `{node_id,outcome,kind,url,error,trace_id}` | Outbound delivery status |
| `chat_title` | `{title}` | Chat title generated |
| `done` | `{}` | Stream complete |
| `error` | `{error}` | Stream error |

### Response Schema

Each chat turn returns a `Turn` with `OutputItem`s:

| Type | Field | Description |
|------|-------|-------------|
| `message` | `text` | Standard LLM reply |
| `quack:dag` | `plan` | DAG structure (nodes, edges) |
| `quack:result` | `text` | Final vetted answer |

## MCP Server

**Streamable-HTTP MCP** is mounted at `/api/v1/mcp`:

- **Protocol**: MCP over HTTP POST with streaming responses
- **Use cases**: IDE integration (VS Code, JetBrains), CLI tools, A2A clients
- **Configuration**: Optional; disabled if `QUACK_MCP_ENABLED` unset

## A2A Integration

**Agent Client Protocol (ACP)** enables external agents to run within Quack's workflow:

| Agent | ACP Command | Permissions |
|-------|-------------|-------------|
| code-implementer | `opencode acp` | Read/write workspace, git commits |
| code-reviewer | `opencode acp` | Read-only, no commits |
| code-explorer | `opencode acp` | Read-only, no commits |

### ACP Agent Flow

1. **Remote agent starts** — `opencode acp` connects to Quack's ACP endpoint
2. **Session bound** — Agent's Name identifies it per-node (`plan.ID:node.ID`)
3. **Tools provided** — Workspace, git, skill tools
4. **Worker loop** — Generate → critique → revise → judge
5. **Gate reads commits** — Quack's vetting layer reads the workspace ledger, not agent self-report

### ACP Advantages

- **External execution** — Run on powerful remote machines (OpenHands, etc.)
- **Separate credentials** — Agent's own git/token, not Quack's
- **Isolated jail** — Workspace confined to `<root>/<chatID>/`
- **Gate ownership** — Delivery (commit/push/PR) remains Quack-owned

## Webhook Extensions

Quack receives inbound webhooks via registered `Extension`s:

### GitHub Integration

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/github/webhook` | POST | GitHub webhook delivery |
| `/github/manifest` | GET | App installation manifest |
| `/github/callback` | POST | OAuth callback |

### Extension Registration

Extensions implement `extension.Extension`:

```go
type Extension interface {
    Name() string
    RegisterRoutes(r *chi.Mux)
    Run(ctx context.Context, cfg config.Extension)
}
```

### GitHub Webhook Payloads

- **Pull request** — Triggers code review DAG
- **Issue creation** — Triggers issue→plan→implement→review workflow
- **Push events** — Can trigger validation workflows

## Frontend Integration

The embedded SPA is built with Vite + React + TypeScript:

### Frontend Structure

| Directory | Purpose |
|-----------|---------|
| `frontend/src/` | Source code |
| `frontend/dist/` | Compiled output (embedded in binary) |
| `frontend/public/` | Static assets |

### Frontend API Client

TypeScript client generated from `openapi.yaml`:

- **Config**: `frontend/openapi-ts.config.ts`
- **Output**: `frontend/src/generated/`
- **Command**: `make generate` (via `scripts/generate.sh`)

### Real-time Updates

- **SSE subscription** — `/api/v1/chats/{chat_id}/stream`
- **Replay buffer** — Reconnect-safe; replays missed events
- **Live streaming** — Events flow from orchestrator → executor → SSE

## Configuration

### Environment Variables

| Variable | Purpose |
|----------|---------|
| `QUACK_SERVER_PORT` | HTTP port (default: 3929) |
| `QUACK_LLM_ENDPOINT` | LLM provider endpoint |
| `QUACK_LLM_API_KEY` | LLM provider API key |
| `QUACK_DATABASE_URL` | Postgres connection |
| `QUACK_QDRANT_URL` | Vector store (optional) |
| `QUACK_MCP_ENABLED` | Enable MCP server |
| `QUACK_EMBED_MODEL` | Embedding model |
| `QUACK_JUDGE_MODEL` | Judge model |
| `QUACK_ORCH_MODEL` | Orchestrator model |

### OpenAPI Generation

```bash
make generate
```

This runs:
1. **oapi-codegen** — Generates `internal/schema/quack.gen.go`
2. **openapi-ts** — Generates `frontend/src/generated/`

## Observability

### OpenTelemetry

- **Metrics**: Traces, spans, counters per workflow stage
- **Tracing**: Full request DAG propagation
- **Logs**: Structured JSON via `slog`

### Debug Endpoints

| Endpoint | Purpose |
|----------|---------|
| `/debug/pprof` | Go profiling (when `QUACK_PPROF_ADDR` set) |
| `/health` | Liveness check |
