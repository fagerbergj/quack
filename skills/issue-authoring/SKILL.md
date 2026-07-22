---
name: issue-authoring
description: >
  How to write a GitHub issue for the quack repo — problem first, verified
  context over guesses, explicit scope, and the labeling taxonomy. Use when
  composing or triaging a GitHub issue.
---

# Issue Authoring

An issue exists to **hand off a well-scoped unit of work** — to a human or to `quack:plan`. A vague wishlist gets re-triaged before anyone can start; a problem statement backed by `file:line` evidence gets planned in one pass.

## Problem first, not a solution wishlist

Open with `## Problem`: the concrete symptom or gap, one paragraph. Not "we should add X" — say what's broken or missing and how you noticed. The solution (if you have one) goes in a later section (`Desired` / `Design` / `Fix shape`) — never replace the problem statement with it.

## Verified context over guesses

Add `## Verified context`: what you actually confirmed by reading the code, each claim anchored to `file:line`. Do the surface-level investigation before filing — grep for the mechanism, read the function, check whether the gap is real — rather than transcribing a hunch. State findings as fact ("X does Y at `foo.go:42`"), not "I think" or "probably".

If something is still unknown, say so explicitly and keep it separate from what's verified — e.g. "Verified: the frontend has no code path for this (grepped `github_url` in `Chat.tsx`, no hits). Not verified: whether the CLI has the same gap — didn't check `internal/cli/`." Never let an unverified guess read like a verified fact.

## Scope

State what's in and explicitly out. For anything with a real failure mode (data loss, security, an easy-to-reach-for wrong default), add `## Out of scope / forbidden` and name what the implementation must never do — e.g. "never prune a session with an in-flight status," "default must be keep-forever." Skip the forbidden section when there's nothing an implementer could plausibly get wrong in a damaging way.

## Separate code from config/deploy

Before filing as a code bug, check whether the actual cause is infrastructure — a proxy timeout, an env var, a Docker/compose setting, a missing secret. If the fix is in `docker-compose.yml`, a deploy script, or a runtime env, say so and flag it for ops instead of describing it as an application bug; don't send an implementer hunting through Go/TS source for a problem that isn't there.

## Both surfaces

If quack has both a web UI and a CLI, check whether the bug or gap applies to one or both — grep the CLI equivalent before assuming it's web-only (or vice versa). State the answer explicitly, even when it's "CLI: confirmed unaffected, see `show.go:141-181`" — silence reads as unchecked, not as "only the web UI."

## Acceptance

Concrete and testable — a reviewer should be able to check each line off without asking you what you meant. For anything touching code, end with the standard bar:

```
`go test ./...`, `npm test`, vet, fmt green.
```

## Labels

Every issue gets exactly one `priority:*` and one `complexity:*`, plus whatever `area:*`/`bug`/`enhancement`/`feature-request` tags apply.

- **`priority:high`** — do next (headline/milestone item, active breakage). **`priority:medium`** — soon, not urgent. **`priority:low`** — nice to have, no one's blocked.
- **`complexity:low`** — small, contained change, one or two files. **`complexity:medium`** — several files or a moderate design call. **`complexity:high`** — cross-cutting, large surface, or needs a design doc first (pair with `needs-design` if it must not go straight to `quack:implement`).

## Prose style

Concrete over generic, no ceremony. Say what you found, not that you "looked into it." Cite the line, don't describe the neighborhood. Skip throat-clearing ("As part of this effort...") and skip filler sections that don't apply to this issue — a two-line bug report keeps only Problem + Verified context + Acceptance.

## Good vs weak

**Weak:** "The chat UI gets slow with lots of tool calls. We should probably optimize the state updates somewhere in the store."

**Good:** "UI performance degrades badly as tool calls stream in. Verified: `appendRunToolCall`/`fillRunToolResult` (`messageParts.ts:92-130`) each do `[...run.activity, newItem]` — a full array copy per event, O(N²) over a run's lifetime. `TurnView` is already memoized (`TurnView.tsx:71`), so the cost is in the state layer, not React re-renders. CLI: `show.go` doesn't accumulate tool-call activity at all, so this mechanism doesn't apply there."

## Anti-patterns

| Don't | Instead |
|---|---|
| Lead with the fix, skip the symptom | `## Problem` first, always |
| "I think it's probably in the store somewhere" | grep it, cite `file:line`, or say plainly what's unverified |
| File a proxy/env/deploy issue as a code bug | flag it for ops; say where the actual knob lives |
| No `priority:*`/`complexity:*` | pick both before filing |
| Acceptance = "works correctly" | acceptance = things a reviewer can literally check |
