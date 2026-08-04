---
type: Workflow Document
title: DAG Planning and Execution
description: How Quack decomposes user requests into Directed Acyclic Graphs of agents, validates plans, executes them as native ADK workflow graphs, handles setup clones with shared/scoped isolation, runs nodes through the trust gate, translates events to SSE, and manages human-in-the-loop pauses. Covers Planner validation, PlanJudge plan scoring, BuildWorkflow construction, and delivery mechanics.
tags: [dag, execution, planning, orchestrator, workflow]
resource: /internal/dag/planner.go
---

# DAG Planning and Execution

When a user submits a request, Quack's orchestrator decomposes it into a **DAG** of agent nodes - the work is parallelized according to dependencies, each node runs through the adversarial trust gate, and verified outputs propagate to dependents before final synthesis.

## Orchestrator

[`internal/orchestrator/orchestrator.go`](/internal/orchestrator/orchestrator.go) drives the workflow: `Orchestrator.Run` takes a user request, delegates to Planner for validation (not generation), then passes the DAG to the DAG executor which runs it as a native ADK workflow graph.

```
User request → Orchestrator.Run → Planner.Validate → BuildWorkflow → dagStream → SSE
```

## Planner Validation

[`internal/dag/planner.go`](/internal/dag/planner.go) implements the `Planner` struct that validates orchestrator-authored DAGs. The orchestrator itself authors the DAG (guided by the plan-work skill in `skills/plan-work/SKILL.md`); the Planner checks it:

- **Known agents** - every node references a registered agent from the roster
- **Unique IDs** - no duplicate node identifiers within a plan
- **Acyclicity** - dependencies form a valid DAG
- **Synthesizer hardening** - the synthesizer's dependencies are validated to ensure it has all required upstream inputs
- **Check command safety** - only configured prefixes from `workspace.check_commands` are allowed

### Plan Judge

The Planner optionally carries a `PlanJudge` instance (`vetting.PlanJudge`). The plan judge scores a proposed DAG against quality criteria:

- Plans are scored before execution begins, replacing the old regex routing backstop
- When judge is disabled (`config.Gates.JudgeEnabled() == false`), `judgeRouting` no-ops rather than blocking validation
- The judge evaluates: verification adequacy, actionability, and honesty (tightened in commit 30fefb4)

### Review Churn Threshold

[`internal/dag/planner.go`](/internal/dag/planner.go) defines `reviewChurnThreshold = 800` changed lines. Above this, a single code-reviewer node reliably chokes on the whole diff (compaction churn + slow re-diffing - a live incident with +1271 lines stalled for 30+ minutes). Above the threshold, the review must fan out into per-file-group explorers feeding one reviewer.

## Workflow Graph Construction

### BuildWorkflow

[`internal/dag/nativegraph.go`](/internal/dag/nativegraph.go) constructs the ADK workflow graph:

1. **Setup phase** - `runPlanSetup` provisions clones per the plan's declared Setup (repo URL, base ref). Nodes that share a repo chain resolve to `SharedRepoScope`; each independent node gets its own `NodeDir`. For a GitHub-originated run, the plan tool replaces whatever Setup the planner declared with deterministic facts (repo/base_ref) the webhook dispatch already stamped off the triggering event - a GitHub-triggered plan's Setup never depends on the model getting them right.
2. **BuildGateNodes** - wraps each node's worker in `vetting.RunGatedRefine`. This is where every agent output passes the trust gate before propagating downstream.
3. **Topological execution** - the plan runs as ONE native ADK workflow graph under one runner with `WithMaxConcurrency`. All nodes share one workflow session (id = chatID), isolated by branch + isolation scope.

### Continue-but-warn on Gate Failure

When a gate-failed dependency is required by a downstream node, execution continues but warns - the dependent receives the unvetted output with a flag so it can account for quality uncertainty in its own work.

## Event Streaming

See also: [Streaming Vocabulary](/architecture/overview.md#streaming-vocabulary)

[`internal/stream/translator.go`](/internal/stream/event.go) implements `Translator` which converts raw ADK session events into the SSE vocabulary shared by REST and MCP transports. The translator tracks per-node state so agent activity is correctly attributed to its node and run.

## Delivery Mechanics

See also: [Streaming Vocabulary](/architecture/overview.md#streaming-vocabulary)

[`internal/stream/event.go`](/internal/stream/event.go) defines `delivery_result` events with four outcome values:

| Outcome | Meaning |
|---------|---------|
| `delivered` | Work reached its destination (PR pushed, review posted) |
| `draft` | Judge passed but a gate-failed dependency carried through; PR opened as draft |
| `failed` | Delivery attempt failed (push rejected, API error, or refused by the trigger's permission grant) |
| `none` | Phantom-success class - judge passed but no delivery attempt was recorded at all |

The gate owns delivery (`commitDelivery` → GitHub extension). It fires exactly once per work item, and a gate-failed PR opens as a draft rather than being discarded. Ground-truth probes in [`internal/vetting/gitprobe.go`](/internal/vetting/gitprobe.go) (`augmentFromRepo`) read commits/changed files off the clone to synthesize staged PRs; [`internal/vetting/answerreview.go`](/internal/vetting/answerreview.go) (`augmentFromAnswer`) parses a reviewer's `VERDICT:`/`FINDINGS:` tail into staged reviews with inline comments.

For a GitHub-triggered run, `commitDelivery` also partitions staged items against `Plan.Grant` (`vetting.Grant`, computed once at webhook dispatch from labels, PR authorship, and a fork check) before any of them reach the extension - this is the run's ONE actual permission enforcement point, not the DAG plan or the worker's own staging calls. An item the grant doesn't cover is refused, logged at error level, and reported as a `failed` `delivery_result`; a plan with no GitHub trigger carries a nil `Grant`, which permits everything.

## Human-in-the-Loop (HITL)

Workers can pause execution by calling the `ask_user` tool. The gate detects this call, pauses the node via `workflow.ResumeOrRequestInput`, and routes the user's answer back when submitted. Uses a stable interrupt key `(invocation, nodeID, round)` - collision-free under ADK v2's resume scoping.

## Recent Changes

- **Plan judge plan scoring** (commit 30fefb4): Tightened the plan quality rubric for verification, actionability, and honesty; replaced regex routing backstop with `vetting.PlanJudge`.
- **Delivery result as OTel signal** (commit be43498): `delivery_result` is now emitted as a durable stream event independent of the judge verdict, distinguishing phantom "gate passed" successes from actual delivery outcomes.
- **Trigger permission grants enforced at delivery** (#675): `commitDelivery` now refuses any staged item the run's `Plan.Grant` doesn't cover, closing a hole where a GitHub-triggered plan could stage (and ship) a review or PR push nothing had actually authorized.
- **Consumer split for GitHub trigger evidence** (#679): `Plan.WorkerBackground` and `Plan.CIChecks` give individual nodes an ask-only slice of a GitHub trigger's context, separate from the orchestrator's full-scale envelope (`internal/github/envelope.go`) - see [`docs/extensions/github.md`](/docs/extensions/github.md) for the envelope shape.

## Related Concepts

See also: [System Architecture](/architecture/overview.md) · [Adversarial Trust Gate](/architecture/vetting.md) · [Workspace Isolation](/architecture/workspace-jail.md)
