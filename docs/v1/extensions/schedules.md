# Schedules extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Start work at set times, such as Sleeper's weekly jobs, without anything outside quack calling it (D21). Core knows only submissions; this extension's service half owns the timers.

**Success.** No scheduled run that is still useful is lost to a restart or redeploy, and every skipped run is visible.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| SCH-1 | MUST | Let extensions and agents declare schedules that submit to a session key at set times. | D21 |
| SCH-2 | MUST | Keep schedules durable across restarts and redeploys. | D21 |
| SCH-3 | MUST | List schedules with their next and last run in the UI's operate view. | D30 |
| SCH-4 | MUST | Let each schedule declare how long a run stays useful (a deadline or a staleness window; default until the next occurrence). After downtime, run a missed run once if it is still useful, merging several misses; otherwise skip it and record it as missed. | SC1 |

