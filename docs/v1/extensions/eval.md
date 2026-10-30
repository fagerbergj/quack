# Eval extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Prove a change before it ships. Every production agent has a benchmark built from its real sessions, and a candidate model, prompt, rubric or extension version runs against it before it reaches prod (D10).

**Success.** No agent change reaches prod without a benchmark result, and a change that loses on both cost and quality does not ship.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| EVL-1 | MUST | Build datasets from recorded sessions and keep datasets and experiment results in Langfuse. | EV1, OBS-4 |
| EVL-2 | MUST | Replay a dataset against a candidate agent configuration (model, extension pins, knowledge pins) and report quality (checker verdicts), cost, turns, revisions and wall time. | D10 |
| EVL-3 | MUST | Provide the benchmark result that core requires before an agent change reaches prod. | OBS-5 |
| EVL-4 | MAY | Run experiments on hosted models; prod roles stay local. | D12 |
| EVL-5 | MUST | Require at least 20 real cases, including known-bad ones, in an agent's benchmark before it reaches prod; add repeats and golden labels when a comparison is a close call. | EV2 |

