# Workflows extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Run repeatable jobs as fixed steps written in code, calling a model only for steps that need judgment. This makes them faster, cheaper and easier to measure than a free agent loop (D35). Sleeper's weekly jobs and reMarkable ingest use it at launch.

**Success.** A workflow resumes after a crash at the step it was on, and every step's status and cost is visible.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| WFL-1 | MUST | Let an extension define a workflow as named steps that run in code, model calls, decision points or gated actions. | D35 |
| WFL-2 | MUST | Commit each step's result before the next starts, and resume after a crash at the interrupted step. | HAR-3, HAR-15 |
| WFL-3 | MUST | Run a workflow inside a session, so its log, cost, checks and follow-ups work like any session's. | D19 |
| WFL-4 | MUST | Show each step's status, duration and cost in the session view. | UI-7 |
