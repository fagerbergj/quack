# The quack CLI

`packages/cli`: operations and scripting against a distributor, no chat TUI. `quack --help` lists every command; `--json` prints the API's JSON unchanged. The server is `--url`, else `--server NAME`, else `$QUACK_URL`, else the current saved server (see [Servers](#servers)), else `http://localhost:8080`. A saved server sends its saved token; otherwise `$QUACK_ADMIN_TOKEN` is sent when set. An API error prints its problem `detail`. Exit codes: 0 ok, 1 error or failed run, 2 not found.

| Command | Does |
| --- | --- |
| `sessions [--state S] [--agent A] [--order-by F] [--page-size N] [--page-token T] [--keys] [q]` | One page of `GET /sessions`; `--keys` prints bare keys. |
| `show [-f] [--verdict] [--messages N] <key>` | The session, its pending asks, the newest version of each artifact kind and the last N messages (default 5, at most 50); `-f` then streams new transcript lines until the run ends. |
| `new [--agent A] [--key K] [-p] <text>` | Start a session through `POST /sessions/:key/invocations` and print its key. |
| `send [-p] <key> <text>` | Send a message; returns once accepted and prints its submission id. A message sent during a run queues as a follow-up. |
| `steer <key> <text>` | Send a message into the running turn instead of queueing it; with no run going it starts one, as `send` does. |
| `withdraw <key> <id>` | Take back a queued follow-up by the id `send` printed; exits 2 when it is not queued. |
| `asks [--session K] [--page-size N] [--page-token T] [q]` | One page of pending approvals and questions, across sessions oldest first or for one session. |
| `answer <key> <askId> <choice> [--text T] [--reason R]` | Resolve an ask: an approval's choice is `approve` or `reject`, a question's is an option label, and `--text` carries a free answer, or a note beside an option label. |
| `approvals`, `approve <key> <askId> [reason]`, `reject <key> <askId> [reason]` | `asks type:approval`, and `answer` with the choice filled in. |
| `artifacts [--page-size N] [--page-token T] [<key>]` | One page of sealed artifact versions, across sessions or for one session, newest first. |
| `artifact <key> <kind> [--version N]` | Print the newest version of a kind, or the one named. |
| `app <name> <path>` | `GET /apps/<name><path>` of an extension app, path and query as the app's API documents them. |
| `stop`, `resume`, `rename`, `archive`, `unarchive`, `delete`, `export` | Housekeeping, as in the web UI. |
| `agents` | Agent versions. |
| `server [add <name> <url> [--token T] \| use <name> \| list [--keys] \| remove <name>]` | Saved servers; `server` alone prints where commands go now and whether it answers. |
| `completion bash\|zsh` | Print a completion script: `source <(quack completion bash)`. Keys complete from `quack sessions --keys`, server names from `quack server list --keys`. |
| `eval <args>` | Runs `packages/eval/src/main.ts` with the arguments as given (needs the `LANGFUSE_*` variables). |

## Servers

`server add` saves a distributor under a name once it answers `GET /version`, and `server use` makes it the one every command reaches without `--url`. `--server NAME` picks another saved one for a single command.

```
$ quack server add prod http://100.64.0.6:18081
saved prod: http://100.64.0.6:18081, version eca6df0c2618 built 2026-11-09T22:15:04Z
$ quack server use prod
current server: prod http://100.64.0.6:18081
$ quack server list
   NAME  URL                      TOKEN
*  prod  http://100.64.0.6:18081
$ quack server
prod  http://100.64.0.6:18081  reachable, version eca6df0c2618 built 2026-11-09T22:15:04Z
```

`--token T` saves an admin token with the server, sent in place of `$QUACK_ADMIN_TOKEN`; `list` says only whether one is saved. `server remove NAME` forgets one, and removing the current server leaves none current. The servers live in `$XDG_CONFIG_HOME/quack/config.json` (`~/.config/quack/config.json` by default), written whole with mode 0600. A config the CLI cannot read is never overwritten: commands warn and fall back to `http://localhost:8080`, and `server add`, `use` and `remove` refuse until it is fixed or deleted.

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

`sessions` hides archived sessions (`-state:archived`) unless it is given a `q`, and `asks` always adds `state:pending`.

`--order-by` names the field the server orders by; each order runs one way, and the server refuses a field its list does not offer:

| List | Orders |
| --- | --- |
| `sessions` | `lastActivity` (the default, newest first), `createdAt` (newest first), `oldestAskAt` (oldest first) |
| `asks` | `createdAt`, oldest first across sessions, newest first with `--session` |
| `artifacts` | `sealedAt`, newest first |

A list prints one page, of at most `--page-size` rows (the server's default 100, capped at 1000). When more remain, `next page: --page-token T` goes to stderr, so stdout still pipes; pass it back with the same query for the next page. With `--json` the token is the page's `nextPageToken`.

## New sessions

`new` mints the key `<agent>:<uuid>`, or `ui:<uuid>` without `--agent`. Pages find their sessions by key prefix, so `quack new --agent contracts 'A REST contract for ...'` makes a `contracts:` session that Contract Studio lists. `--key K` starts the session at that key instead, `quack new --key contracts:sessions-v11 --agent contracts '...'`, and refuses a key that exists: send to that one with `send`.

## Verdicts

`show --verdict` adds the judge's verdict on each kind's newest version, when it has one: passed or failed, the score, the rubric and its version, the failed gates, and each failed check with its first note cut to one line. Verdicts are not on the REST artifact rows, so `--verdict` reads them from the session's socket, which starts a stopped session's process as opening it in the web UI does. `--json` adds them as `verdicts`.

```
$ quack show --verdict 'github:fagerbergj/quack#2238'
...
Verdicts
github.review v1  passed  score 0.87  github.reviewer@7
  constructive_actionable.says_why: finding-1: The finding explains the mechanism of the false positive ...
```

## Extension apps

`app <name> <path>` reads any route an extension app serves under `/apps/<name>`, so no app needs a command of its own. A body that is one list of flat rows, such as a page of the Contracts app API, prints as a table, and anything else as indented JSON; `--json` prints the body unchanged. When the page has a `nextPageToken`, stderr says to add `pageToken=T` to the path.

```
$ quack app contracts '/contracts?q=pendingApproval:true'
KEY                  TITLE                  KIND        DRAFT  PROPOSED  APPROVED  CHANGESREQUESTED  PENDINGAPPROVAL
contracts:aab002bf…  Judge evidence API: …  typescript  6      6         4         5                 true
```

## Print mode

`new -p` and `send -p` wait for the run to end and print only the final assistant text on stdout, so the output pipes. `new -p` writes `session <key>` to stderr. A run that does not finish (`unanswered`, a model fault) prints the reason on stderr and exits 1. The wait is one HTTP request held open, so a proxy with a short timeout in front of the distributor ends it early.
