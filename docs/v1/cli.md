# The quack CLI

`v1/packages/cli` (D23): operations and scripting against a distributor, no chat TUI. `quack --help` lists every command; `--json` prints the API's JSON unchanged. The URL is `--url`, else `$QUACK_URL`, else `http://localhost:8080`; `$QUACK_ADMIN_TOKEN` is sent when set. Exit codes: 0 ok, 1 error or failed run, 2 not found.

| Command | Does |
| --- | --- |
| `sessions [--state S] [--agent A] [--keys]` | List sessions; `--keys` prints bare keys. |
| `new [--agent A] [-p] <text>` | Start a session through `POST /sessions/:key/invocations` and print its key. |
| `send [-p] <key> <text>` | Send a message; returns once accepted. |
| `show [-f] <key>` | Summary of a session; `-f` then streams new transcript lines until the run ends. |
| `stop`, `resume`, `rename`, `archive`, `unarchive`, `delete`, `export` | Housekeeping, as in the web UI. |
| `approvals`, `approve`, `reject`, `artifacts`, `artifact`, `agents` | Inbox, decisions, sealed artifacts, agent versions. |
| `completion bash\|zsh` | Print a completion script: `source <(quack completion bash)`. Keys complete from `quack sessions --keys`. |
| `eval <args>` | Runs `packages/eval/src/main.ts` with the arguments as given (needs the `LANGFUSE_*` variables). |

## Print mode

`new -p` and `send -p` wait for the run to end and print only the final assistant text on stdout, so the output pipes. `new -p` writes `session <key>` to stderr. A run that does not finish (`unanswered`, a model fault) prints the reason on stderr and exits 1. The wait is one HTTP request held open, so a proxy with a short timeout in front of the distributor ends it early.

Not carried over from v0: memory, TUI, node controls, sandbox, ledger.
