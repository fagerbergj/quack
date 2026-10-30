# Plans extension

Status: draft, 2026-10-30. Not needed by any launch workload. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Run a large project whose work genuinely parallelizes across agents with different jobs. A planner session splits the work, starts a session per part, follows them, and assembles the result (D32, D34). Everyday work does not use it: a session is one agent loop in one role.

**Success.** A plan finishes faster than one session doing the same work in sequence, at comparable quality, on a benchmark.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| PLN-1 | MUST | Be opt-in per agent. | D32 |
| PLN-2 | MUST | Let a planner session start child sessions with their own agent roles, run them in parallel, and message, steer, resume or stop them. | D34 |
| PLN-3 | MUST | Show the plan and each child's status in the planner session. | HAR-7 |
| PLN-4 | MUST | Keep each child's external effects behind its own gated actions. | D3 |
