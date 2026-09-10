# CLI

`quack` is a one-binary CLI and server. There is no TUI: `-p`, `chat send`, and `chat show` are the interface, and their pause/failure exit codes (`0` answered, `1` failed, `2` paused on a question) make them pipeable and scriptable. Every command also runs against a running server over HTTP + SSE (`--server`, or the active one from `quack server add`/`use`) - there's nothing local-only about it except `server run` itself.

Every command has its own `--help`; this page is the map.

## Shell completion

`quack completion <bash|zsh|fish|powershell>` generates a completion script. Chat, node, memory, and server ids/names complete dynamically (a live call against the target server or local config); `sandbox --agent` completes from the local `quack.yaml`.

## Getting configured

| Command | Does |
| --- | --- |
| `quack init` | Onboarding wizard: run a server locally (writes `quack.yaml`; the CLI then runs it in-process, no server registered) or register a remote one someone else runs. |
| `quack server init` | Just the config wizard - LLM provider, endpoint, model roles, optional features, stores. Writes `quack.yaml` without touching the client registry. `--answers <file.yaml>` skips the wizard entirely for a headless setup (Dockerfile, CI, Ansible) - a YAML file of the same fields the wizard asks for (see `InitAnswers` in `internal/cli/emit.go`); unset fields fall back to the same environment variables the wizard prefills from. |
| `quack server use <name>` / `add <name> <url>` / `list [--json]` / `remove <name>` | Manage the set of servers this CLI knows about and which one is active. `add` activates the server if it's the first one registered. |
| `quack server validate [--json]` | Load and validate a `quack.yaml` without starting the server. |
| `quack server login <name> --issuer <url> --client-id <id>` | Log in to a registered server that requires [OIDC auth](configuration/auth.md#cli-login-quack-server-login), via the authorization code flow with PKCE (needs a local browser - doesn't work headless/over SSH). |

Once logged in, `quack chat`/`quack api`/`-p` attach the stored access token to every request against that server automatically (refreshed silently as it nears expiry) - nothing else to pass on the command line.

## Running the server

`quack server run` runs the REST + MCP API and the embedded SPA in the foreground (`--config`, `--port`). See [deployment shapes](configuration/deployment.md) for the local-vs-Docker-vs-remote options.

## One-shot prompts

```bash
quack -p "Research the best time to visit Dublin"
```

Prints the answer and exits. Flags: `--events` (also print the pipeline trace - plan, node lifecycle - to stderr), `--attach` (attach a file, e.g. an image, repeatable), `--json` (one JSON result object instead of plain text).

## Chats

A chat is a session; a message on it kicks off a run.

| Command | Does |
| --- | --- |
| `quack chat new` | Create a chat, print its id. |
| `quack chat send <id> "<msg>"` | Send a message (also how you answer a paused question). |
| `quack chat show <id> [-f]` | Status snapshot - id/title/status/pending question, node table, last answer; `-f` follows a live run. Applies the same 0/1/2 exit-code contract as `chat send`/`-p` (`--json` too). |
| `quack chat list [--archived exclude\|include\|only]` | List chats with their status. `--archived` defaults to `exclude`; `only` is the CLI's one path to a chat archived in the web UI. |
| `quack chat export <id>` | Export a chat transcript. |
| `quack chat stop <id> [--json]` | Stop a chat's active run. |
| `quack chat delete <id> [--json]` | Delete a chat (irreversible). The confirmation prompt goes to stderr; a non-interactive stdin without `-y` errors instead of silently declining. |
| `quack chat rename <id> <title>` | Rename a chat. |
| `quack chat archive <id>` / `unarchive <id>` | Archive or unarchive a chat. |
| `quack chat artifact list <id>` | List a chat's artifacts and their revision history. |
| `quack chat artifact download <id> <name> [--revision N] [-o file\|-]` | Download one artifact revision's bytes (default: the artifact's own name, sanitised to a bare local filename). `-o -` streams to stdout instead of writing a file. |

## Node control

Mid-run control over one node in the active DAG (`quack chat node <verb> <chat-id> <node-id> ...`); every verb takes `--json`, printing `{"chat_id","node_id","action","message"}`.

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

## Recording, replay, and eval

Runs can be recorded to a replay ledger and re-driven later - the basis for regression-checking a prompt or model change against real traffic.

| Command | Does |
| --- | --- |
| `quack ledger list` / `export <chat-id>` | List chats with a recording on the server; download one as a bundle for replay or a fixture. |
| `quack ledger show <chat-id> [--from-seq N]` | Print a chat's raw ledger entries (server-side, from the local quack.yaml's stores). |
| `quack ledger recover [chat-id] [--dry-run] [--json]` | Settle intents whose projection write is missing (the same pass the server runs at boot); `--dry-run` reports only. |
| `quack ledger rebuild <chat-id> [--dry-run] [--json]` | Reconcile a chat's artifact metadata and SSE table against the ledger fold. |
| `quack replay [--from-server <url>]` | Replay a recorded run offline (strict) or live from a changed node (fork). `--from-server` names where to fetch the recording when the argument is a chat id - distinct from the global `--server` (which this command doesn't otherwise use; the replay itself always runs from your local `quack.yaml`). |
| `quack eval [--from-server <url>]` | Re-run a recorded conversation live with a swapped model and compare judge scores. Same `--from-server` meaning as `replay`. |

## Memory

Browse or invalidate what quack has remembered (memory lifecycle design doc); `forget` is the CLI verb for the manual-delete remediation path a memory-poisoning incident needs.

| Command | Does |
| --- | --- |
| `quack memory list [--bucket <b>] [--q <query>] [--tier <t>] [--sort <s>] [--limit N] [--include-invalidated]` | List or (with `--q`) embedding-search memories. With no `--limit`, auto-pages through the whole store rather than stopping at the server's default page. |
| `quack memory show <memory-id>` | Show one memory's full detail (votes/tier/last-recalled) - `GET /api/v1/memories/{id}`, a direct per-id lookup, not a store-wide scan. |
| `quack memory forget <memory-id> [--reason <text>] [--json]` | Invalidate (soft-delete) one memory. |
| `quack memory sweep [--dry-run]` | Run the forgetting-rule sweep on demand (epic #1255 P3); `--dry-run` reports per-rule matches without invalidating anything. |
| `quack memory rescope [--apply]` | Move role:\* memories with a resolvable GitHub-origin chat into their repo:\* bucket (#1262); dry run by default. |
| `quack memory stats [--weeks N]` | Weekly recall precision/support-share/vote/recall counts plus live/invalidated points per scope (epic #1255 P5); defaults to 12 weeks. |

## Sandbox

`quack sandbox` enters or probes the real agent jail - the same caps, argv wrapping, and spawn env an ACP agent gets, which is what makes it useful for reproducing "works for me, fails in the jail" bugs.

| Command | Does |
| --- | --- |
| `quack sandbox` | Enter the jail interactively. |
| `quack sandbox run "CMD ARGS"` | Run one command inside the jail and exit. |
| `quack sandbox info [--json]` | Print the resolved jail (mode, cwd, tmp, home, grants, env) without running anything. |
| `quack sandbox check [--json]` | Run the built-in jail probes; non-zero exit on any FAIL. `--json` emits the same rows as a machine-readable array. |

See [`docs/sandbox-cli.md`](sandbox-cli.md) for the detail.

## Misc

`quack version` prints the version. `quack git-askpass` is a helper quack invokes for itself during git operations - not something you run directly; it reads the credential from `QUACK_GIT_ASKPASS_USERNAME`/`QUACK_GIT_ASKPASS_TOKEN`, set by the calling process for that one invocation.

## Raw API access

`quack api [method] <path>` is a `gh api`-style passthrough to the REST API - `quack api /health`, `quack api POST /api/v1/chats -d '{"system_prompt":"..."}'`, `-d @body.json` or `-d -` for stdin. Targets the active server (or `--server`); with neither configured, it runs the duck in-process. See [`docs/api.md`](api.md) for the full REST/MCP/A2A surface this rides on.
