# GitHub extension

Status: draft, 2026-10-30. Priority 2 of the launch workloads (D2). Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Act on the operator's repositories as a GitHub App: review pull requests, implement issues, fix failing CI or review findings, and answer questions in threads (G4).

**Shape.** One session per PR or issue (D19). Each run takes the agent role its trigger asks for, so `/review` runs as reviewer and `/fix` as implementer over the same log and workspace (D33).

**Success.** Review p90 within 10 minutes including checker time (D9), with review quality at least v0's on the same PRs (PRD release plan).

**Out of scope.** Reviewing every PR or push automatically (G1).

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| GH-1 | MUST | Receive and verify GitHub App webhooks in the service half, and map each to the session key of its PR or issue. | SUR-3, D19 |
| GH-2 | MUST | Start the first review when a configured label is added; start later runs with `/review` (reviewer) and `/fix` (implementer) comment commands; start implement-from-issue when an issue is assigned to the App; answer in a thread when a PR or issue comment mentions the App. | G1, G5 |
| GH-3 | MUST | Before a review starts, give the reviewer the repository's conventions (AGENTS.md or CLAUDE.md, lint configuration, the linked issue) and the blast radius of each changed symbol (callers, implementations, tests that touch it). | G2 |
| GH-4 | MUST | Let the reviewer run any command in its session workspace. | G3 |
| GH-5 | MUST | Support four jobs: PR review, implement from an issue (open a PR), fix failing CI or review findings on a PR (push commits), answer questions in PR and issue threads. | G4 |
| GH-6 | MUST | Make every write to GitHub (review, comment, push, pull request) a gated action checked by the judge extension against an audited rubric for its job. | D3, D11 |
| GH-7 | MUST | Act with short-lived installation tokens injected at run time. | NFR-SEC-3 |

## Uses from core

Triggers and session keys, role per run, gated actions, yield hook, decision points, workspace with toolchains (RUN-1), egress allowlist for GitHub and package registries (D25).

