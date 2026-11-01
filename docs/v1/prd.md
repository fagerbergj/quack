# Quack v1 PRD

Status: draft for review, 2026-10-30. Every requirement traces to [decisions.md](decisions.md) (`D*` core, `R*`/`G*`/`S*`/`RM*`/`MEM*`/`J*`/`EV*` per extension), to the operator's platform requirements (`P`), or, for rows that pin architecture, to an `A*` entry. Normative detail is in the [SRS](srs.md); each extension has its own short document under [extensions/](extensions/).

## Summary

Quack v1 is a small, durable agent core that runs structured, repeatable work on local models and lets nothing leave until it has been checked. Everything else, including the judge, memory, research, GitHub, Sleeper and reMarkable, is an extension on that core.

## Problem

Quack v0 works, and its checked delivery and review quality beat what the local 27B model does on its own. But it is slow, cluttered and tied to one machine. These problems share six causes:

1. **No core primitive.** Each feature (DAG, run log, ledger, records, memory, artifacts, decisions, vetting) has its own storage and screens, and durability was added after the fact. So every feature adds persistence, API and UI, and the judge can't be pulled out of the orchestrator.
2. **The server, not the session, is the unit.** Sandboxes, clones and runs all belong to one process on one host, so nothing can move or scale out.
3. **Quality is bought with model calls.** Judge-and-revise loops, shell probing and model-mediated recall multiply tokens because the harness gives agents neither the evidence up front nor a way to see what they spend. A code review costs millions of tokens; v0's review p90 was about 43 minutes against a 10-minute target.
4. **Many ways to extend, no stable contract.** Bundles, plugins, extensions (in-process and two out-of-process protocols), skills, a workflow catalog and a registry, with domain code still inside core.
5. **The UI mirrors internals.** It shows nodes, rounds and artifacts because they exist, not because a user needs them.
6. **Features without a definition of value.** Memory and artifacts never had a success criterion, so nothing showed they weren't helping.

The things to keep are gated, structured delivery; one core that extensions add experiences to; review quality above the base model; and quack as a playground for agent ideas.

## Users

**The operator** (one person) runs quack on a home server with local models (qwen 27B workers and judge, Clef for decisions, via llm-swap), uses it from a desktop or phone browser, and develops it. Ad-hoc, unchecked work stays in pi; quack is for work worth checking and repeating (D1, D5).

**Guests** may join a session to watch, drive or approve (D8, D39). No v1 workload needs them; multiplayer is built to explore, not for launch.

**Non-human participants** start and continue work: GitHub webhooks, schedules, other sessions and checkers (D8).

Teams, per-team budgets and acting as a delegated user identity are out of v1 (D1).

## Goals

1. **Checked delivery.** Any external effect (a PR comment, a pushed commit, a delivered report, a stored memory) can be held until it passes its checks or the operator approves it; each agent declares which ones are (D3, D26, D41).
2. **A small core that everything extends.** The judge, memory, planning, workflows, schedules and every workload are extensions on one versioned contract; core has no domain or rubric knowledge (D14–D17, D21, D27).
3. **Durable sessions.** A crash, redeploy or days of idleness never loses committed work or repeats completed steps (platform requirements, kept by D1).
4. **Fast enough to use.** p90 per session, including checker time: code review 10 minutes, research report 8, one checker round 3 (D9, D46).
5. **Measured changes.** Agents are easy to create and try as drafts, but only an agent that passes a benchmark built from real sessions is promoted to run unattended, and a promoted agent's changes must match or beat it (D10, D48, D50, D59).
6. **A calm UI built around jobs.** Four jobs: start and follow work, approve, read artifacts, operate (D30).
7. **Cloud-shaped from day one.** It runs on one home server, but sessions are isolated units behind a container contract, so another host or AgentCore needs no agent or extension code change (D1, D4).

## Non-goals

- Parity with v0. Only features the operator uses or finds plausibly useful are rebuilt (D2).
- Migrating v0 data. v1 starts empty; v0's database is archived read-only (D13).
- Teams, budgets that block work, spend alerts, delegated user identity (D1, D40).
- An in-app usage dashboard; trends are read in Langfuse and Grafana (D31).
- A chat TUI; the CLI is for operations and scripting (D23).
- Hosted models in prod roles, except a per-agent opt-in fallback (D12).
- Unchecked ad-hoc chat and research, which stay in pi (D5).

## User jobs

### 1. Start and follow work

The operator asks a question or opens a session in the web UI, or a trigger (a GitHub label or comment, a schedule) starts one. They watch it live: streamed output, tool calls, pending approvals, who is present and who drives. They can steer the run, queue a follow-up or abort, and the session stays consistent. One unit of work (a PR, a research question, a Sleeper week) is one session, so later runs see the history and workspace of earlier ones (D19, D33).

### 2. Approve

Everything waiting on a human, across all sessions, is in one inbox. Each item shows the full action (what goes where, as whom), the checks it passed or failed, and the evidence behind them. Approving or rejecting with a reason takes one step; a rejection goes back to the agent. A web push says when something is waiting (D22, D26).

### 3. Read artifacts

Reports, reviews and Sleeper suggestions are artifacts: finished products, listed across sessions and read as documents rather than transcript fragments, each with its versions and check status. A research report shows its sources, and can be exported or sent to the reMarkable (R6). Intermediate data never becomes an artifact, which is what cluttered v0 (D42).

### 4. Operate

The operator browses agents (fixtures shipped by extensions, files, and their own), creates and forks them in the UI, diffs versions and promotes them once benchmarked; runs and compares benchmarks, sees per-session cost at cloud-equivalent prices, and handles sessions that are parked after crash loops or failed. The CLI does the same for scripts (D23, D29, D30, D48, D50).

## Launch workloads

In priority order (D2). Each is an extension with its own document.

| Priority | Workload | Shape | What it does |
| --- | --- | --- | --- |
| 1 | [Research](extensions/research.md) | Session | Checked deep reports with verified citations from the web, code, papers and the operator's documents |
| 2 | [GitHub](extensions/github.md) | Session | PR review, implement from an issue, fix CI or review findings, answer threads |
| 3 | [Sleeper](extensions/sleeper.md) | Workflow | Lineup, waivers, trades, league reports and predictions on a schedule, shown on the Sleeper page |
| 4 | [reMarkable](extensions/remarkable.md) | Workflow | Transcribe, file and extract actions from chosen handwritten documents |

Platform extensions that the workloads use: [approvals](extensions/approvals.md), [judge](extensions/judge.md), [memory](extensions/memory.md), [eval](extensions/eval.md), [plans](extensions/plans.md), [workflows](extensions/workflows.md), [schedules](extensions/schedules.md).

## Success measures

| Measure | Target | Source |
| --- | --- | --- |
| p90 session time, including checks | review 10 min, research 8, checker round 3 | D9, D46 |
| Lost committed work on crash or redeploy | none | SRS NFR-HAR-1 |
| Changes shipped without a benchmark | none | D10, D27 |
| External effects missing from the audit record | none | D26, D41 |
| Domain or rubric code in core | none | D3, D26 |
| Time to answer "why did this take N rounds, what failed, what did it cost" | one screen or one command | D30 |
| Judge agreement with itself under replay | at least 95% of rounds, 98% of criteria | D45 |
| Judge changes on golden sets | no added false passes, false fails or slowdown | D45 |
| Saturated rubric criteria | none at maximum on more than 90% of real rounds | D45 |

Everything else (cache hit rates, tool rejections, revision counts, cost) is a diagnostic read in Grafana to explain a missed target, never a target itself (D45).

## Release plan

1. **Core with the judge, approvals and eval, proven on the GitHub extension**, so v1 reviews its own pull requests as early as possible (D51). v0 keeps running.
2. **GitHub switch.** The v1 reviewer replays 10 recent pull requests that v0 reviewed, with deliveries blocked, and must hold up against v0's reviews (D53). Then the GitHub App points at v1 and v0's reviewer retires, with no side-by-side period (D52).
3. **Research**, still first by value (D2), and the web UI's four jobs.
4. **Sleeper**, with workflows and schedules.
5. **reMarkable**, then memory and plans.
6. **v0 switched off**, and its database archived read-only.

Each step ships only when its benchmark passes (D10). The first reviewer's 10-case benchmark is a bootstrap exception (D53); EV2's 20-case bar holds for later promotions.

## Open questions

None at the product level. Extension documents list their own.
