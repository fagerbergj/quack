# Memory extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Keep a small set of facts the operator has approved and give them to the agents they concern. v0's agent-written memory mostly stored and recalled pointless facts; v1 stores nothing without the operator's approval (D7).

**Success.** Memory earns its place on a benchmark: an agent with memory beats the same agent without it, or memory is turned off for that agent (MEM2).

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| MEM-1 | MUST | Let agents propose a memory as a gated action; it is stored only when the operator approves it. | D7 |
| MEM-2 | MUST | Scope each memory: global, per agent, or per subject (e.g. a repository or a league). | MEM1 |
| MEM-3 | MUST | Render a small pinned set of memories as a stable prompt section in matching sessions, and offer the rest through a recall tool. | MEM1 |
| MEM-4 | MUST | Let the operator list, edit and delete memories. | D7 |
| MEM-5 | MUST | Let an agent run its benchmark with and without memory, and turn memory off for an agent that does not win. | MEM2 |

