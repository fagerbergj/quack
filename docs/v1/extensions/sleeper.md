# Sleeper extension

Status: draft, 2026-10-30. Priority 3 of the launch workloads (D2). Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Help the operator manage their fantasy football team: weekly lineup and waiver advice, trade ideas and analysis, league reports, and stat predictions, all on one page in quack (S1, S2).

**Shape.** Weekly jobs are workflows: data fetching and statistics in code, a model only for analysis and writing (D35). Follow-ups on a result open a session (S2).

**Success.** Suggestions and predictions are scored against what actually happened (S4); predictions stay observe-only until they beat the platform's projections (S1).

**Out of scope.** Posting to the league chat or making roster moves; push notifications for weekly results (S2).

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| SL-1 | MUST | Provide these weekly jobs: lineup, waivers, trade finder, trade analysis, trade talk (with give and get), league report, league history, player trends, predictions. | S1 |
| SL-2 | MUST | Run each weekly job as a workflow on its schedule, and let the operator start any of them from the Sleeper page. | D35, S3 |
| SL-3 | MUST | Show results on the Sleeper page as cards, inside the quack UI shell. | S2, UI-5 |
| SL-4 | MUST | Let any card open a session to ask follow-up questions about it, with the card's data in context. | S2 |
| SL-5 | MUST | Keep daily league snapshots, past suggestions and their outcomes indefinitely, and use them in analysis and to score suggestions and predictions. | S4 |
| SL-6 | MUST | Keep predictions observe-only (shown, not used in advice) until their measured accuracy beats the projections. | S1 |
| SL-7 | MUST | Read the Sleeper API only; the extension has no writes to Sleeper. | S2 |
| SL-8 | MUST | Check analysis output with the judge extension against audited rubrics before it appears on the page. | D3, D11 |
| SL-9 | MUST | Finish each scheduled job at least 1 hour before the deadline it serves (the first kickoff a lineup covers, waiver processing); finish on-demand jobs within a 3-minute p90. | S5 |

## Uses from core

Workflows and schedules extensions, decision points (routable to Clef), the service half for the page and league history, the session follow-up path.

