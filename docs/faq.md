# FAQ

Design questions that come up often - the *why* behind a few choices that look surprising from the outside.

## Why do the coding agents run as external subprocesses, not inside quack?

The coding inner loop is a commodity. Quack's value is the orchestration and the trust gate around a task, not the diff-aware edit tools, LSP navigation, and context compaction that a mature coding agent already ships. Rather than rebuild all that, quack spawns an external agent ([pi](https://github.com/earendil-works/pi) by default) and talks to it over the [Agent Client Protocol](https://agentclientprotocol.com) (`internal/acp`). The agent has **no quack tools** - the gate reads its work off the git clone and its answer, so quack stays the authority on what's trusted without owning the editor.

## Can we swap the coding agent?

Yes - that's the point of speaking ACP. "Which coder" is a config choice (`agents.<name>.acp.command` in `config/quack.yaml`): the same client drives pi, Claude Code's agent, or gemini-cli with a one-line change.

Why pi specifically? We evaluated [`earendil-works/pi`](https://github.com/earendil-works/pi) as a leaner alternative to opencode (the original default) - its tool core is genuinely minimal and its footprint is far smaller. The one blocker was ACP: pi has no first-party ACP mode, so the `tools/pi-acp` shim (`pi-acp.mjs`) implements the ACP subset quack's executor uses and drives `pi --mode rpc` underneath - including the two things that are load-bearing for delivery safety: `git push` denied inside the subprocess (delivery is gate-owned) and the reviewer/explorer held read-only. Quack's own workspace sandbox (`bwrap`/`landlock` via `internal/workspace`) backs the filesystem boundary. The swap was worth it on footprint alone: ~145MB node_modules vs the 170MB binary opencode carried, and roughly a third of the idle/peak RSS per turn.

## What is the trust gate's design lineage?

[Self-Refine](https://arxiv.org/abs/2303.17651) (the self-refine pass), [LLM-as-a-Judge](https://arxiv.org/abs/2306.05685) (rubric scoring with concrete, decomposed criteria), and [the case for adversarial agents](https://jatinmishra27.medium.com/when-your-ai-needs-an-enemy-the-case-for-adversarial-agents-30a906b2273b) (why independence beats self-checking) are the patterns the gate leans on directly. [Reflexion](https://arxiv.org/abs/2303.11366) (language reflections stored in memory) and [CRITIC](https://arxiv.org/abs/2305.11738) (tool-grounded critique) are noted for later - out of scope for now, along with escalation to a larger judge model for high-stakes nodes.

## Why not just use ADK's stock `LoopAgent` for the trust gate?

`LoopAgent` (generate → critique → refine, `max_iterations` + one `escalate` exit) assumes its sub-agents are native ADK agents sharing one session, coordinating through `output_key`/state placeholders. Quack's actual workers structurally aren't that: the code agents run as an OS subprocess (`internal/acp/proc.go`) - the `pi-acp` shim driving pi - with no ADK session at all - the gate reads their output off the git clone and their answer, not off shared session state. There's no state for a loop to hand between iterations without first writing a custom ADK-agent shim around the subprocess, at which point `LoopAgent` isn't saving anything.

Independent of that: `RunGatedRefine` runs three separately-budgeted stages (continuation, deterministic checks, judge/revise) where `LoopAgent` gives one loop and one stopping rule, and the judge itself runs in its own isolated runner (`session.InMemoryService()`) specifically so it's walled off from the worker's session - the opposite of a shared-session critic loop. Native (non-ACP) bundles could in principle use `LoopAgent` for a bare self-refine pass, but the judge and checks stages don't shrink to fit there either, so it'd be a second refine mechanism to maintain for no savings even in the one place it could apply.

## Why does the GitHub App use stdlib crypto + `golang-jwt`, not `go-github`/`ghinstallation`?

The App flow is one signed JWT plus a handful of REST calls (mint an installation token, post a comment, open a PR, resolve an installation). `golang-jwt/jwt/v5` is already in the module graph and the REST calls are small `net/http` requests. Pulling in `go-github` (a large generated client) and `ghinstallation` to save ~80 lines is a poor trade for a self-hosted binary - so: `jwt/v5` for the RS256 JWT, stdlib `net/http` for the REST, stdlib `crypto/hmac` for the webhook signature.
