# Judge extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Check an artifact against a rubric and either pass it, send it back for revision with feedback, or let it go out with its failures marked. This is the part of v0 that makes reviews better than the base model's; in v1 it lives outside core (D3).

**Success.** One checker round within 3 minutes at p90 (D9). Under replay the judge agrees with itself on at least 95% of rounds and 98% of criteria; a judge change adds no false passes, false fails or slowdown on golden sets; and no rubric criterion is at its maximum on more than 90% of real rounds (D45).

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| JDG-1 | MUST | Check an artifact against the rubric its agent pins, at the yield hook (continue the run with feedback) or as a resolver in an approval chain. | D17, APR-1 |
| JDG-2 | MUST | Port v0's rubrics only after each one has been audited. | D11 |
| JDG-3 | MUST | Version rubrics as knowledge, separately from the extension's code, and pin them per agent. | D27 |
| JDG-4 | MUST | Allow at most the revise-and-recheck rounds the agent's manifest declares, then let the artifact go out with each failed criterion marked. | J1, R3 |
| JDG-5 | MUST | Raise decision points for individual judgments, so each can be routed to a different resolver. | D15 |
| JDG-6 | MUST | Record every verdict, with criteria results and evidence, so the UI can answer "why N rounds and which check failed" and the eval extension can replay it. | UI-7, D10 |

