# Research extension

Status: draft, 2026-10-30. Priority 1 of the launch workloads (D2). Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Answer an ad-hoc question with a sourced report whose claims and citations were checked before the operator sees it. Quick, unchecked research stays in pi (D5).

**Shape.** One session per question, in the researcher agent role, without the plans extension (D32, R5).

**Success.** A checked deep report at p90 within 8 minutes including checker time (R8), and no report shown as checked whose citations do not support its claims.

**Out of scope.** Scheduled topic watches, push notifications per report beyond core's "artifact ready" push, shareable public links.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| RES-1 | MUST | Before researching, raise a decision point on whether the question is ambiguous enough to ask first. If so, ask the operator a few clarifying questions; otherwise start and state the assumptions in the report. | R1 |
| RES-2 | MUST | Use these sources: web search and fetch (pages and PDFs), code repositories and docs sites, academic papers, past quack reports, and transcribed reMarkable documents. | R2, R7 |
| RES-3 | MUST | Check every report with the judge extension against an audited research rubric before it is shown as a finished artifact. | D3, D11 |
| RES-4 | MUST | When the checker's revision limit is reached with checks still failing, deliver the report with each failure marked inline and a "not fully checked" banner. | R3, J1 |
| RES-5 | MUST | Keep the session open after delivery. The agent decides each follow-up's shape: by default a direct, checked answer in the session; a follow-up that changes the findings produces a new report version. | R4 |
| RES-6 | MUST | Provide a report reading view with sources and check status, Markdown and PDF export, and send-to-reMarkable as a gated action. | R6, RM3 |
| RES-7 | MUST | Search and cite past reports and reMarkable transcripts through core's artifact index. | R7, A15 |

## Uses from core

Yield hook (the judge continues the run with feedback), decision points (RES-1), gated actions (send to reMarkable), code mode and subagent questions for fan-out (D36, D37), egress allowlist covering search, fetch and paper APIs (D25).

## Open

- Which search and paper providers to use is an implementation choice for the extension's design.
