---
type: Vetting Architecture
title: Adversarial Trust Gate
description: The trust gate that every DAG node's output must pass before propagating downstream — self-refine loop, deterministic checks (citations, length, build/vet/test), and independent judge scoring. Covers RunGatedRefine, PlanJudgeScope isolation, setupCloneReadOnly for the plan judge, fail-closed on budget errors, and deterministic-check skip signals via OTel metrics.
tags: [vetting, trust-gate, adversarial-judge, self-refine]
resource: /internal/vetting/node.go
---

# Adversarial Trust Gate

Every DAG node's output passes a **trust gate** before it counts as final and flows downstream or to the user. This is the core quality mechanism — nothing a node produces is trusted by default. Limited local models bluff, so adversarial verification is the guardrail.

## The Generate→Critique→Revise→Judge Loop

[`internal/vetting/node.go`](/internal/vetting/node.go) implements `RunGatedRefine`, the reusable core that wraps any worker agent in a gated refine loop:

```
Worker draft → Deterministic checks → Independent judge → Revise (loop until score >= threshold or rounds exhausted)
```

### 1. Self-Refine (Free Polish Pass)

The worker critiques and revises its own output in one pass — [Self-Refine][self-refine] pattern. Cheap, but it shares the worker's blind spots, so it is a polish pass, not the trust decision.

[self-refine]: https://arxiv.org/abs/2303.17651 "Self-Refine"

### 2. Deterministic Checks

Mechanical checks that can be run on the output without another model call: citation backing, answer length, delivery/review criteria, and — for code nodes — build/vet/test commands derived from the clone on disk. Implemented in [`internal/vetting/checks.go`](/internal/vetting/checks.go).

The `checksPassCriterion` function runs checks via `workspace.RunPipeline`, which is argv-only (no shell) and bound by an operator-configured allowlist (`workspace.check_commands`). It stops at the first failure, folding a bounded head of output into the revise feedback.

Check commands are **derived from the repo**, not guessed: the planner authors the DAG before looking at any codebase, so derived checks run once the worker has cloned and the repo exists on disk. Derived checks query standard indicators (e.g., presence of `go.mod`, `package.json`, `Makefile`) and map them to appropriate commands (`go build`, `npm test`, etc.).

### 3. Independent Judge

A separate, independently-configured judge model scores the output using G-Eval style rubric — one score per criterion (0–10 integer scale), with an overall verdict of the **weakest criterion**. The judge's system prompt includes standing criteria (grounded claims, no fabrication) plus per-node acceptance criteria written by the planner at DAG construction time.

The judge terminates by calling `submit_verdict` — a structured tool, never by emitting prose or JSON in its reply text. This prevents ambiguity about when the verdict is final.

If a judge round scores below `cfg.Threshold` (default 0.7, normalized from the integer scale), the worker receives concrete revision feedback and runs again, up to `cfg.JudgeRounds` times.

### 4. Revise Loop

On judge failure, the worker's revise round receives the judge's criterion-level feedback and its own best guess at how to fix it. The loop continues until: score passes threshold, max rounds exhausted, or `ErrNodeEmpty` is returned (worker produced no answer).

## Node Control and HITL

[`internal/vetting/node.go`](/internal/vetting/node.go) provides cooperative control primitives:

- **Cancellation** — checked at gate-stage boundaries; mid-call cancel isn't possible on ADK v2 without breaking the shared event stream. The tool layer closes that window: a cancelled node's next tool call is refused outright via `tools.Deps.NodeCancelled / cancelguard.go`.
- **Human-in-the-loop (HITL)** — the `ask_user` tool (`AskToolName`) lets a worker pause for user input. The gate detects the call and pauses the node via `workflow.ResumeOrRequestInput`, routing the user's answer back to this node on the next turn. Uses a stable interrupt key: `(invocation, nodeID, round)` is collision-free under ADK v2's resume scoping.

## Plan Judge Isolation

Recent changes ([`internal/tools/setup.go`](/internal/tools/setup.go) — `SetupCloneReadOnly`, [`internal/workspace/jail.go`](/internal/workspace/jail.go) — `PlanJudgeScope`) introduced isolated repo provisioning for the plan judge:

- **`PlanJudgeScope`** (`"quack-plan-judge"`) is a reserved node identifier that the plan judge resolves its grounding clone into. It is deliberately distinct from `SharedRepoScope` so the judge's read-only clone can never race with or be clobbered by the DAG's own clear-then-clone of the same repo once nodes actually run.
- **`SetupCloneReadOnly`** clones a repo at `baseRef` for inspection only — no branch checkout, no committer identity config. It clears stale targets first (idempotent) and uses `context.Background()` because git clone is not cancellable via ctx (future-proofing for when the API accepts ctx).

## Recent Changes

- **Budget-bounded judge prompts** (commit 5ef3a39): The judge prompt is now budgeted, and errors fail closed — the gate treats unbounded-judge failures as pass-fail rather than letting them silently succeed.
- **Score sinking on unread clones** (commit e1b3538): Exploration nodes that never read what they clone get their score sunk automatically via deterministic checks.
- **Union-derived checks across toolchains** (commit 0816912): Derived check commands now union across present toolchains instead of stopping at the first detection.
- **Deterministic-check skip as OTel signal** (commit be43498): Skip reasons for `checksPassCriterion` are now recorded as queryable OTel attributes and metrics — "checks passed" and "checks never ran" are distinguishable in Tempo/Grafana.

## Configuration

Judge behavior lives in [`internal/vetting/judge.go`](/internal/vetting/judge.go) system prompt construction:

- `judgeBehaviourHead` — core adversarial stance ("You did NOT write the answer being evaluated")
- `judgeNoToolsClause` / `judgeReadToolsClause` — tool capability selection; read tools (read_file, list_dir, glob, grep) let the judge ground code-quality scores in actual artifacts
- `judgeSkillsClause` — optional skill loading for the judge so it references the same quality principles as the worker
- `defaultJudgeMaxIterations = 14` — bounds the agentic tool loop when `Config.JudgeMaxIterations` is unset
- `judgeBehaviourTail` — scoring discipline: score every criterion, never penalize unfamiliar content as fabrication, always terminate with `submit_verdict`

## Related Concepts

See also: [System Architecture](/architecture/overview.md) · [Workspace Isolation](/architecture/workspace-jail.md) · [DAG Execution](/workflows/dag-execution.md)
