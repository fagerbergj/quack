# Approvals extension

Status: draft, 2026-10-30. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Decide who gives the verdict on each gated action. Core guarantees that a gated action waits and is audited; this extension holds the policy (D26).

**Success.** Every gated action reaches a verdict through a declared path, and the operator can read that path in the agent's configuration.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| APR-1 | MUST | Let each agent declare, per gated action, an ordered chain of resolvers (e.g. deterministic check, judge, human) where each approves, rejects or escalates. | D16, D26 |
| APR-2 | MUST | Route human verdicts to session participants with the approver role. | D39 |
| APR-3 | MUST | Let an action with no approval chain configured proceed without waiting; it is still audited. | D41 |
| APR-4 | MUST | Allow a participant to approve an action their own input caused. | D38 |

## Uses from core

Gated actions, decision points and resolver chains, session roles, the approvals inbox and web push in the UI.
