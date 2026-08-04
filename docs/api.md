# API

[`../openapi.yaml`](../openapi.yaml) is the actual contract - codegen (`make generate`) drives both the Go server stubs (`internal/schema`) and the TypeScript client (`frontend/src/generated/`) from it, so it's the source of truth for every path, request/response shape, and the SSE event vocabulary (`x-sse-events`). This page is a map, not a copy.

## REST

Mounted at the process root (`internal/server/router.go`), modeled after OpenResponses: a chat holds turns, each turn is a `Response` with typed `output` items (a standard `message` item plus quack's own `quack:dag` and `quack:activity` extensions).

| Endpoint | Does |
| --- | --- |
| `GET /health` | Liveness check. |
| `GET` / `POST /api/v1/chats` | List / create chats. |
| `GET` / `PATCH` / `DELETE /api/v1/chats/{chat_id}` | Get (with turns), rename, or delete a chat. |
| `POST /api/v1/chats/{chat_id}/responses` | Send a message; streams the run as SSE. |
| `GET /api/v1/chats/{chat_id}/responses/{response_id}` | Fetch one turn's output items. |
| `GET /api/v1/chats/{chat_id}/stream` | Reattach to a chat's in-progress (or most recent) run - replays, then streams live. |
| `PUT /api/v1/chats/{chat_id}/responses/{response_id}/status` | Cancel the active run. |
| `PUT /api/v1/chats/{chat_id}/nodes/{node_id}/status` | Transition a DAG node: cancel, pause/resume, retry. |
| `PATCH /api/v1/chats/{chat_id}/nodes/{node_id}` | Edit a not-yet-started node's prompt. |
| `POST` / `PATCH` / `DELETE /api/v1/chats/{chat_id}/nodes/{node_id}/queue[/{message_id}]` | Queue, edit, or remove a message for a running node - delivered at its next turn boundary, never mid-turn. |

This is what `quack chat` / `quack chat node` ([`cli.md`](cli.md)) and the [web SPA](ui.md) both ride.

## Streaming

`POST .../responses` and `GET .../stream` both emit `text/event-stream`: `event: <name>` followed by `data: <json>`. The DAG (`dag_plan` + `node_*` events) is the static structure; within a node, the trust gate runs a sequence of agent invocations ("runs") - the worker draft, each judge round, each revision - each delimited by `agent_start`/`agent_complete` and carrying a `run_id` + `stage` (`worker`/`judge`/`revise`).

Event names: `response_created`, `agent_start`, `agent_thinking`, `agent_tool_call`, `agent_tool_result`, `agent_token`, `agent_complete`, `dag_plan`, `node_queued`, `node_start`, `node_done`, `node_failed`, `node_cancelled`, `node_needs_input`, `node_paused`, `node_steered`, `delivery_result`, `chat_title`, `done`, `error`. Full shapes are on `sendChatMessage`'s description and `x-sse-events` in `openapi.yaml`.

## MCP

Mounted at `/api/v1/mcp` (Streamable HTTP, `internal/server/mcp`) - this is how Opencode and Claude Code drive quack.

## A2A - internal, not a client-facing face

A2A is currently an **internal** orchestrator↔agent protocol, not something an outside client talks to. Each agent bundle runs its own A2A server on an ephemeral loopback port (`internal/agent/a2a.go`); the orchestrator is an A2A client to its own agents, in-process today and promotable to a standalone address later with no change to the agents themselves. There's no public A2A endpoint.

## GitHub App

A fourth client sits in front of the REST API over its own webhook, not one of the above: the [GitHub App](extensions/github.md). It drives runs via the `quack:plan` / `quack:implement` / `quack:review` / `quack:merge` / `quack:fix` label workflow, `/quack` mentions, and (on PRs it authored itself) review engagement with no label at all - replying on the issue/PR when the run completes.

## Auth

See [configuration/auth.md](configuration/auth.md): OIDC bearer tokens for direct API/MCP clients, trusted headers behind a forward-auth gateway.
