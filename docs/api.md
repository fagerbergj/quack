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
| `PUT /api/v1/chats/{chat_id}/nodes/{node_id}/status` | Transition a DAG node: pause/resume, retry. Cancel is `.../stop`. |
| `POST /api/v1/chats/{chat_id}/nodes/{node_id}/start` | Explicit per-node start: queued or paused into running. |
| `POST /api/v1/chats/{chat_id}/nodes/{node_id}/stop` | Explicit per-node stop: any non-terminal status into cancelled. |
| `PATCH /api/v1/chats/{chat_id}/nodes/{node_id}` | Edit a not-yet-started node's prompt. |
| `POST` / `PATCH` / `DELETE /api/v1/chats/{chat_id}/nodes/{node_id}/queue[/{message_id}]` | Queue, edit, or remove a message for a running node - delivered into a worker round in flight at once (a native node's round is interrupted and continues with it), else at the node's next turn boundary. |
| `GET /api/v1/chats/{chat_id}/artifacts` / `GET .../artifacts/{artifact_name}` | List a chat's artifacts, or fetch one's current bytes. |
| `GET .../artifacts/{artifact_name}/revisions` / `GET .../artifacts/{artifact_name}/diff` | List an artifact's revision history, or diff two revisions. |
| `GET` / `DELETE /api/v1/memories/{memory_id}`, `GET /api/v1/memories`, `POST .../vote`, `POST /api/v1/memories/sweep`, `POST /api/v1/memories/rescope`, `GET /api/v1/memories/stats`, `GET .../nodes/{node_id}/memories` | The `quack memory` surface ([`cli.md`](cli.md#memory)): get/list/search, forget, vote, sweep, rescope, stats, and one node's memories. |
| `GET` / `POST /api/v1/plugins`, `DELETE /api/v1/plugins/{name}`, `GET /api/v1/plugins/updates`, `POST /api/v1/plugins/{name}/update`, `POST /api/v1/plugins/update` | The dynamic plugin registry (epic #1427, [`agent-plugins.md`](agent-plugins.md#the-registry)): list every registered plugin, register a `github:` entry (fetched synchronously), remove one, check every `github:`-sourced row for a newer remote sha, and fetch one row or every behind row. `github:` entries only - a local root stays config-only (`plugins.seed`). |

A `POST .../responses` body may carry `a2ui_action` (`surface_id`, `name`, `source_component_id`, `context`) with an empty `content`: a click on an A2UI surface's button. The turn's user text becomes the line `[a2ui_action] {compact JSON}`. On an extension (`ext:`) chat, a REST turn runs under the delivery grant of that chat's latest dispatch.

This is what `quack chat` / `quack chat node` / `quack memory` ([`cli.md`](cli.md)) and the [web SPA](ui.md) both ride. `GET /api/v1/config`, `GET /api/v1/extensions`, `GET /api/v1/recordings`, and `GET .../chats/{chat_id}/recording` are SPA-internal (settings/extensions panels, recording browser) and not part of the CLI-facing surface above.

## API compatibility version

`openapi.yaml` carries two integers under `info`: `x-api-version` and `x-api-min-version`. `make generate` copies them into `internal/schema/apiversion.gen.go` and `frontend/src/generated/apiVersion.gen.ts`, and `GET /api/v1/config` reports them as `api_version` and `api_min_version`. The SPA compares the version it was built against with the server's on load and on every SSE reconnect:

- Server `api_min_version` above the SPA's version: the page is covered by a "reload" prompt, because its calls would fail.
- Server `api_version` different from the SPA's, or the server's build `version` changed since the page loaded: a non-blocking "quack was updated" strip offers a reload.

Bump rule:

- A change that breaks an existing client bumps `x-api-version`: removing or renaming a path, method, parameter, or required response property; adding a required request field or parameter; narrowing a request enum or type; renaming, removing, or reshaping an SSE event in `x-sse-events`.
- Set `x-api-min-version` to the new `x-api-version` when the server stops accepting the old shape. Leave it lower only while the server still serves older SPAs correctly, for example during a deprecation window.
- Additive changes (a new path, an optional request field, a new response property) and removing an optional response property bump nothing; clients must already tolerate an absent optional field.

CI's `openapi-lint` job runs `scripts/check-api-compat.sh <base>` on pull requests. It runs `oasdiff breaking` against the base branch's spec and fails when oasdiff reports an error-level change and `x-api-version` did not increase. oasdiff does not read `x-sse-events`, so SSE vocabulary changes need the bump by hand.

## Streaming

`POST .../responses` and `GET .../stream` both emit `text/event-stream`: `event: <name>` followed by `data: <json>`. The DAG (`dag_plan` + `node_*` events) is the static structure; within a node, the trust gate runs a sequence of agent invocations ("runs") - the worker draft, each judge round, each revision - each delimited by `agent_start`/`agent_complete` and carrying a `run_id` + `stage` (`worker`/`judge`/`revise`).

Event names: `response_created`, `agent_start`, `agent_thinking`, `agent_tool_call`, `agent_tool_result`, `agent_token`, `agent_complete`, `dag_plan`, `node_queued`, `node_admitted`, `node_start`, `node_done`, `node_failed`, `node_cancelled`, `node_needs_input`, `node_paused`, `node_steered`, `artifact_revision`, `artifact_judge_round`, `delivery_result`, `compaction`, `chat_title`, `done`, `error`. Full shapes are on `sendChatMessage`'s description and `x-sse-events` in `openapi.yaml`.

## MCP

Mounted at `/api/v1/mcp` (Streamable HTTP, `internal/server/mcp`) - this is how Claude Code and other MCP clients drive quack.

## A2A - internal, not a client-facing face

A2A is currently an **internal** orchestrator↔agent protocol, not something an outside client talks to. Each agent bundle runs its own A2A server on an ephemeral loopback port (`internal/agent/a2a.go`); the orchestrator is an A2A client to its own agents, in-process today and promotable to a standalone address later with no change to the agents themselves. There's no public A2A endpoint.

## GitHub App

A fourth client sits in front of the REST API over its own webhook, not one of the above: the [GitHub App](extensions/github.md). It drives runs via the `quack:plan` / `quack:implement` / `quack-auto-review` / `quack:merge` / `quack:fix` label workflow, `/quack` mentions, and (on PRs it authored itself) review engagement with no label at all - replying on the issue/PR when the run completes.

## Auth

See [configuration/auth.md](configuration/auth.md): OIDC bearer tokens for direct API/MCP clients, trusted headers behind a forward-auth gateway.
