# CLI

`quack` is a one-binary CLI and server.
There is no TUI: `-p`, `chat send`, and `chat show` are the interface, and their pause/failure exit codes (`0` answered, `1` failed, `2` paused on a question) make them pipeable and scriptable.
Every command also runs against a running server over HTTP + SSE (`--server`, or the active one from `quack server add`/`use`) - there's nothing local-only about it except `server run` itself.

Every command has its own `--help`; this page is the map.

## Getting configured

| Command | Does |
| --- | --- |
| `quack init` | Onboarding wizard: run a server locally (writes `quack.yaml`, then registers `localhost`) or register a remote one someone else runs. |
| `quack server init` | Just the config wizard - LLM provider, endpoint, model roles, optional features, stores. Writes `quack.yaml` without touching the client registry. |
| `quack server use <name>` / `add <name> <url>` / `list` / `remove <name>` | Manage the set of servers this CLI knows about and which one is active. |
| `quack server login <name> --issuer <url> --client-id <id>` | Log in to a registered server that requires [OIDC auth](configuration/auth.md#cli-login-quack-server-login), via the authorization code flow with PKCE (needs a local browser - doesn't work headless/over SSH). |

Once logged in, `quack chat`/`quack api`/`-p` attach the stored access token to every request against that server automatically (refreshed silently as it nears expiry) - nothing else to pass on the command line.

## Running the server

`quack server run` runs the REST + MCP API and the embedded SPA in the foreground (`--config`, `--port`).
See [deployment shapes](configuration/deployment.md) for the local-vs-Docker-vs-remote options.

## One-shot prompts

```bash
quack -p "Research the best time to visit Dublin"
```

Prints the answer and exits.
Flags: `--events` (also print the pipeline trace - plan, node lifecycle - to stderr), `--attach` (attach a file, e.g. an image, repeatable), `--json` (one JSON result object instead of plain text).

## Chats

A chat is a session; a message on it kicks off a run.

| Command | Does |
| --- | --- |
| `quack chat new` | Create a chat, print its id. |
| `quack chat send <id> "<msg>"` | Send a message (also how you answer a paused question). |
| `quack chat show <id> [-f]` | Status snapshot - id/title/status/pending question, node table, last answer; `-f` follows a live run. |
| `quack chat list` | List chats with their status. |
| `quack chat export <id>` | Export a chat transcript. |
| `quack chat stop <id>` | Stop a chat's active run. |
| `quack chat delete <id>` | Delete a chat (irreversible). |

## Node control

Mid-run control over one node in the active DAG (`quack chat node <verb> <chat-id> <node-id> ...`):

| Verb | Does |
| --- | --- |
| `stop` | Stop a running node; the rest of the run continues. |
| `pause` | Suspend a running node, keeping its accumulated work (resumable). |
| `resume` | Resume a paused node (a fresh re-run, like retry). |
| `queue <message>` | Queue a message for a running node, delivered at its next turn boundary. |
| `queue-edit <message-id> <text>` | Edit a not-yet-delivered queued message. |
| `queue-remove <message-id>` | Remove a not-yet-delivered queued message. |
| `edit <task>` | Edit a not-yet-started node's prompt. |
| `retry` | Re-run a finished node (done/failed/cancelled) and everything downstream of it. |

## Raw API access

`quack api [method] <path>` is a `gh api`-style passthrough to the REST API - `quack api /health`, `quack api POST /api/v1/chats -d '{"system_prompt":"..."}'`, `-d @body.json` or `-d -` for stdin.
Targets the active server (or `--server`); with neither configured, it runs the duck in-process.
See [`docs/api.md`](api.md) for the full REST/MCP/A2A surface this rides on.
