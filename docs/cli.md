# The quack CLI

`packages/cli`: operations and scripting against a distributor, no chat TUI. `quack --help` lists every command; `--json` prints the API's JSON unchanged. The URL is `--url`, else `$QUACK_URL`, else `http://localhost:8080`; `$QUACK_ADMIN_TOKEN` is sent when set. An API error prints its problem `detail`. Exit codes: 0 ok, 1 error or failed run, 2 not found.

| Command | Does |
| --- | --- |
| `sessions [--state S] [--agent A] [--order-by F] [--page-size N] [--page-token T] [--keys] [q]` | One page of `GET /sessions`; `--keys` prints bare keys. |
| `show [-f] [--messages N] <key>` | The session, its pending asks, the newest version of each artifact kind and the last N messages (default 5, at most 50); `-f` then streams new transcript lines until the run ends. |
| `new [--agent A] [-p] <text>` | Start a session through `POST /sessions/:key/invocations` and print its key. |
| `send [-p] <key> <text>` | Send a message; returns once accepted and prints its submission id. A message sent during a run queues as a follow-up. |
| `steer <key> <text>` | Send a message into the running turn instead of queueing it; with no run going it starts one, as `send` does. |
| `withdraw <key> <id>` | Take back a queued follow-up by the id `send` printed; exits 2 when it is not queued. |
| `asks [--session K] [--page-size N] [--page-token T] [q]` | One page of pending approvals and questions, across sessions oldest first or for one session. |
| `answer <key> <askId> <choice> [--text T] [--reason R]` | Resolve an ask: an approval's choice is `approve` or `reject`, a question's is an option label, and `--text` carries a free answer. |
| `approvals`, `approve <key> <askId> [reason]`, `reject <key> <askId> [reason]` | `asks type:approval`, and `answer` with the choice filled in. |
| `artifacts [--page-size N] [--page-token T] [<key>]` | One page of sealed artifact versions, across sessions or for one session, newest first. |
| `artifact <key> <kind> [--version N]` | Print the newest version of a kind, or the one named. |
| `stop`, `resume`, `rename`, `archive`, `unarchive`, `delete`, `export` | Housekeeping, as in the web UI. |
| `agents` | Agent versions. |
| `completion bash\|zsh` | Print a completion script: `source <(quack completion bash)`. Keys complete from `quack sessions --keys`. |
| `eval <args>` | Runs `packages/eval/src/main.ts` with the arguments as given (needs the `LANGFUSE_*` variables). |

## Queries and pages

`sessions` and `asks` take a Lucene `q` as their trailing arguments, over the fields the Sessions API documents for that list. `quack sessions 'pendingAsks:>0 AND agent:chat'` is one; a reserved character in a value, such as the `:` in a key, is escaped with a backslash or the value is quoted. The options join the query with AND, and the CLI escapes the values it takes from them:

| Option | Adds |
| --- | --- |
| `--state running` | `state:running` |
| `--state needs_you` | `pendingAsks:>0` |
| `--state failed` | `lastRun.outcome:failed` |
| `--state done` | `(state:idle OR state:stopped) AND pendingAsks:0` |
| `--agent A` | `agent:A` |
| `asks --session K` | reads `GET /sessions/K/asks` in place of `GET /asks` |

`sessions` hides archived sessions (`-state:archived`) unless it is given a `q`, and `asks` always adds `state:pending`. `--order-by` takes `lastActivity` (the default), `createdAt` or `oldestAskAt`. A list prints one page, of at most `--page-size` rows (the server's default 100, capped at 1000). When more remain, `next page: --page-token T` goes to stderr, so stdout still pipes; pass it back with the same query for the next page. With `--json` the token is the page's `nextPageToken`.

## Print mode

`new -p` and `send -p` wait for the run to end and print only the final assistant text on stdout, so the output pipes. `new -p` writes `session <key>` to stderr. A run that does not finish (`unanswered`, a model fault) prints the reason on stderr and exits 1. The wait is one HTTP request held open, so a proxy with a short timeout in front of the distributor ends it early.
