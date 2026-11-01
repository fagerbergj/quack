# Audit: memory-agent and the agents' memory.md

- **Sources:** v0 `agents/memory-agent` (prompt 237 words, rubric 675, card 136) on `main`; `agents/orchestrator/memory.md` (the extractor's "what to remember" source); `memory.md` of code-explorer (111 words) and code-implementer (130)
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)). Nothing was run and no model was called.
- **v1 side:** [Memory requirements](../extensions/memory.md) (MEM-1..MEM-5), [Memory RFC](../rfc/extensions/memory.md), D7, MEM1, MEM2
- **Outcome:** drop the extractor agent, its hook and its rubric. Port its quality bar as the description of `memory_propose`. Drop the code agents' `memory.md` files. Memory stays off for every agent until MEM-5's with-and-without benchmark shows a win.

## Effectiveness

No measure of the extractor exists. Its rubric says so itself: "not currently wired to a judge gate", so no output of it was ever scored.

| Evidence | Source | What it shows |
| --- | --- | --- |
| The orchestrator would not call `commit_memory` reliably; regex mining (#542) was rejected as brittle; a model extractor behind a keyword pre-filter replaced it | 8d6cc94b (#546, issue #262) | The reason for the agent is a tool-calling failure of the orchestrator, which v1 does not have (no router, D32). |
| `orchestrator.user_memory_hook` ships off by default (`QUACK_USER_MEMORY_HOOK_ENABLED` unset means false), one model call per qualifying turn | `config/quack.yaml` | Whether it runs in a deployment is a deployment question; this audit did not check any deployed config. |
| "v0's agent-written memory mostly stored and recalled pointless facts" | `docs/v1/extensions/memory.md` | The spec's own finding. The repo holds no count behind it, so this audit cannot confirm or size it. |
| The code agents' `memory.md` asks for `Worth remembering:` lines and says the system stores them once the answer passes | explorer and implementer `memory.md` | No result is recorded for what was stored or whether recall helped a later run. |

## Alignment

The extractor writes without asking: #546 commits candidates through `memory.Store.Commit`, scoped to `user:<login>`, with no approval step. D7 and MEM-1 forbid that; in v1 an agent proposes and the operator approves through the gate. The same applies to the `Worth remembering:` mechanism in the code agents.

Instruction by instruction:

| Instruction | Decision |
| --- | --- |
| Reply with only a JSON array; `[]` is the common answer | Drop. v1 proposes with a tool call (`memory_propose`); proposing nothing is not calling it. Keep the idea in one sentence: most turns state nothing worth keeping. |
| `kind`: preference, goal or limit | Drop. v1 scopes a memory (global, agent, subject; MEM-2), and nothing reads a kind. |
| Normalize to a stable third-person sentence "so duplicate detection works" | Keep one line. v1 has no dedup requirement, but near-identical proposals waste the operator's approval queue. |
| The bar (rubric notes): explicit, not inferred; durable, not transient; a real work or communication preference, goal or standing constraint; non-sensitive; one fact each | Port. This is the useful part, about 60 words as the `memory_propose` description, so every agent with memory sees it. |
| "Never extrapolate from a one-off request; do not guess from tone or brevity" | Keep, folded into the bar. |
| "Skip health, legal, financial, political" | Keep. The operator approves, but a proposal that asks them to review it is still noise. |
| "Invoked once at the end of a turn ... you are never seen by them" | Drop. There is no hook agent. |

The rubric's two criteria (`fact_supported`, `durable_and_scoped`) restate the bar, so they add no check beyond it. In v1 the check is the operator's approval plus the MEM-5 benchmark, so no judge rubric is ported.

The code agents' `memory.md` files say: recall for this repo arrives in a `<MEMORY>` block "useful, but verify before relying on it, since a repo can change under you." That is the only line with content. In v1 pinned memories render in a stable prompt section scoped to the repository (MEM-1, MEM-3); put the "verify before relying" caveat in that section's header. The rest (what counts as tradecraft) is covered by the bar above.

## Conciseness

237 words in the prompt, of which about 60 survive as the bar. The rubric (675 words) and card (136) go. The two `memory.md` files (241 words) go.

## Porting decision

1. `memory_propose` description: the five-part bar and the one-fact-normalized-sentence rule. Nothing else from `memory-agent`.
2. No extractor agent, no pre-filter, no end-of-turn hook. If a post-turn proposer is wanted later, it needs its own benchmark under MEM-5; v0 left none to port.
3. Memory off by default per agent; an agent turns it on only after the with-and-without run (MEM-5).
4. Dropped: the memory-agent bundle and card, its rubric, `kind`, the JSON-array contract, both code-agent `memory.md` files.
