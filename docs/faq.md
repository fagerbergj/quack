# FAQ

Design questions that come up often — the *why* behind a few choices that look
surprising from the outside.

## Why do the coding agents run as external subprocesses, not inside quack?

The coding inner loop is a commodity. Quack's value is the orchestration and the
trust gate around a task, not the diff-aware edit tools, LSP navigation, and
context compaction that a mature coding agent already ships. Rather than rebuild
all that, quack spawns an external agent (`opencode` by default) and talks to it
over the [Agent Client Protocol](https://agentclientprotocol.com) (`internal/acp`).
The agent has **no quack tools** — the gate reads its work off the git clone and
its answer, so quack stays the authority on what's trusted without owning the editor.

## Can we swap the coding agent?

Yes — that's the point of speaking ACP. "Which coder" is a config choice
(`agents.<name>.acp.command` in `config/quack.yaml`): the same client drives
opencode, Claude Code's agent, or gemini-cli with a one-line change.

We looked at [`earendil-works/pi`](https://github.com/earendil-works/pi) as a
leaner alternative — its 4-tool core is genuinely minimal and it's actively
maintained. The blocker is ACP: pi has **no first-party ACP mode** yet (tracked in
its discussion #4444), only an unofficial third-party bridge that forks three ways
and self-admits gaps in filesystem delegation and permission handling. Those two
things — denying `git push`, holding reviewer/explorer to read-only — are
load-bearing for delivery safety, so an unofficial adapter is a worse risk than
opencode's first-party `opencode acp`. Worth a bakeoff if pi ships native ACP with
permission parity; not before.

## Why does the GitHub App use stdlib crypto + `golang-jwt`, not `go-github`/`ghinstallation`?

The App flow is one signed JWT plus a handful of REST calls (mint an installation
token, post a comment, open a PR, resolve an installation). `golang-jwt/jwt/v5` is
already in the module graph and the REST calls are small `net/http` requests.
Pulling in `go-github` (a large generated client) and `ghinstallation` to save ~80
lines is a poor trade for a self-hosted binary — so: `jwt/v5` for the RS256 JWT,
stdlib `net/http` for the REST, stdlib `crypto/hmac` for the webhook signature.
