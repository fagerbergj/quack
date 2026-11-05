# Proposal: one conversation per agent in a shared session

Status: proposal, not built. Prompted by the dogfood run on fagerbergj/games#41.

## Problem

A PR session (`github:owner/repo#n`) is one process on one `session.sqlite` with one root conversation. When `/fix` follows a review, the distributor's `switchRole` (409 "mid-run" if a run, approval or sleep is open) calls `driver.rebind`, which rewrites the agent snapshot and restarts the process. `openSession` then calls `harness.root()` and, seeing stale instructions, `configure()`s the same root conversation. The implementer starts with the reviewer's whole transcript. On #41 it replayed the reviewer's `checkout_pr` (twice, "not available"), never called `checkout_task` (which returns the open review feedback), and edited `./repo` by hand. A transcript is memory and also a few-shot prompt; here the examples belonged to another role.

## What is shared today

State lives in documents, all declared `scope: 'conversation'`: `github.pr`, artifacts, gate, questions, failures. The review reaches `/fix` through `checkout_task`, which reads the PR's reviews from GitHub, not through the transcript. Documents already carry every fact the roles exchange. What hurts is the shared transcript, tool list and instructions.

## Options

1. **Keep one conversation, mitigate.** The trigger text names `checkout_task` first and an unknown-tool error lists the agent's own tools (both in this PR). Cheap and covers this failure, but any role switch still feeds one role's history to the other (reasoning, compaction summaries, cwd assumptions).
2. **Fork per role.** pi-durable's `fork(entryId)` gives the parent's entries up to `entryId` plus a fresh provider session id. Forking from the end repeats the problem; forking from the start equals a fresh conversation.
3. **One conversation per (session, agent), documents shared.** Each role gets a conversation on its first run with its own `pi.agent` (model, instructions, tools). Facts cross only as documents or an explicit hand-off input.

## Recommendation

Ship option 1 now; build option 3 next. Per agent: transcript, tools, instructions, `pi.live`, usage, inbox. Shared across the session: artifacts, `github.pr`, questions, failures, the workspace directory. A role's first input on a new conversation is built by the integration (for GitHub, the trigger text plus a pointer such as "review artifact v3 is in the session"), never by copying history.

## What core must gain (generic only)

- **Conversation per role.** `openSession` resolves a conversation by (session, agent name) instead of `harness.root()`, creating it on first use. The agent name is already in the session's `agent` record.
- **Session-wide documents.** pi-durable has only conversation scope. Core needs a convention for an extension to declare a doc session-wide: it lives on the session's first conversation and role conversations read and write it by that id.
- **Hand-off input.** A submission may carry `{ agent, input }`; the integration decides the text.

Nothing about PRs, reviews or checkout enters core; `github.pr`, the review artifact kind and the trigger text stay in `packages/github`.

## What breaks

- **Session view and WS:** they watch one conversation. They need the active role's conversation plus a role switcher; docs panels read the shared conversation.
- **Replay and export (`/export`, judge replay):** they walk the root. They must take a conversation per role. A review replay should no longer depend on a later `/fix` having extended the transcript, which is an improvement.
- **Eval:** eval sessions run one agent, so one conversation. Fixtures that seed `github.pr` must seed the shared-docs conversation.
- **`/fix` seeing the review artifact:** unchanged in practice (it reads GitHub via `checkout_task`). With artifacts as shared docs, `/fix` can also read the sealed review directly, so it no longer relies on the review having been posted.
- **Busy rule:** per-conversation busy state would allow a concurrent reviewer and implementer, but they share one process and workspace. Keep the 409.

## Migration of existing PR sessions

No rewrite. The existing root holds both roles' history: treat it as the shared-docs conversation and as the transcript of the role it last ran. On the first run by a different role, create that role's conversation fresh. Documents already sit on root, so nothing moves. The role-to-conversation map is a document on root; absent means "root belongs to the recorded agent", so old logs need no version bump.

## Open questions

- Whether per-conversation `pi.usage` is enough for the session cost line, or the view must sum across roles.
- Whether a role conversation is dropped or kept when its agent definition changes (today history is kept).
