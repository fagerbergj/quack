# Quack v1 decisions

Settled answers from the requirements grilling. The PRD and SRS are compiled from this log; a later entry that reverses an earlier one says which.

## Problem framing (2026-10-30)

Root causes behind v0's slowness, clutter and single-machine limit:

1. No core primitive: each feature (DAG, runlog, ledger, records, memory, artifacts, decisions, vetting) has its own storage and UI, and durability was retrofitted.
2. The server, not the session, is the unit of scale and isolation.
3. Quality is bought with model calls (judge loops, shell probing, model-mediated recall) instead of evidence and budgets.
4. Several extension mechanisms and no stable contract; domain code leaks into core.
5. The UI mirrors backend internals instead of user jobs.
6. Features shipped without a definition of value (memory, artifacts).

Keep: gated, structured delivery; one core with extensions layered on it; review quality above what the base model does alone; quack as a playground for ideas.

## Open


## Architecture leanings (for the RFC, not requirements)

- **A1 Process per session.** An always-on distributor authenticates, maps requests to session keys, starts or wakes the owning session process, and routes to it. Each session process loads its pinned extensions in-process; there are no per-extension child processes. A failing extension can crash only its own session, which the durable log resumes.
- **A2 Two halves per extension** (confirmed by the operator). Session half (tools, hooks, sections, tasks) runs in the session process. Optional always-on service half (ingress, schedules, UI pages, cross-session state) can affect sessions only by submitting to a session key; it may run inside the distributor at home or separately when distributed. The distributor holds no domain logic.
- **A3 Layers.** `cloud-agent-layers.svg` is the reference: surfaces and triggers, dispatch, deployment, runtime (one per session), harness, extensions, model access, with identity/governance and observability/evaluation spanning every layer. Extension service halves sit in the surfaces layer.
- **Open for the RFC:** how a session process gets its extensions: compiled into a per-agent image (Go), loaded at start (TypeScript, pi-durable), or WASM.


## Decisions

- **D1 Audience.** One operator on home hardware with local models. Cloud properties from the platform requirements (per-session isolation, container contract, durable log) are hard design constraints so quack can move to AgentCore/ECS later. Teams, per-team budgets and delegated user identity are out of v1.
- **D2 Workloads.** v1 rebuilds every workload, in priority order: research, GitHub, Sleeper, reMarkable. Only features the operator uses or finds plausibly useful carry over; parity with v0 is not a goal.
- **D3 Gate.** Core guarantees that every external effect is a gated action that waits durably for a verdict. Checkers (judge, CI, deterministic checks, a human) are extensions; core knows nothing about rubrics.
- **D4 Deployment.** v1 runs on one home server. The design must allow distribution (sessions on other hosts) without code changes to agents or extensions.
- **D5 Research.** Research in quack means checked deep reports: an ad-hoc question answered with a sourced report whose claims and citations are verified before delivery. Unchecked ad-hoc research stays in pi.
- **D6 Surface.** The web UI (desktop and phone browser) is the primary surface and is designed first. CLI and other surfaces are secondary.
- **D7 Memory.** Memory is an extension, not core. Agents may propose memories; none is stored until the operator approves it through the gate.
- **D8 Participants.** Sessions are multiplayer: several authenticated humans plus non-human participants (triggers, checkers) with roles. In practice one human uses it; this is built as a capability to explore, not a launch need.
- **D9 Speed.** v0's p90 targets become v1 SLOs, measured per session including checker time: code review 10 min, research 8, one checker round 3. (Synthesis dropped by D46.)
- **D10 Evaluation.** Evaluation is a launch requirement. Every run is recorded so it can be replayed; an eval extension builds benchmarks from real sessions and A/Bs models and prompts. No agent goes to prod without a benchmark.
- **D11 Checking criteria.** v0's rubrics define what "checked" means and carry over, but each is audited before porting.
- **D12 Model fallback.** Prod roles run on local models. An agent's manifest may opt in to a hosted fallback model; off by default, and every fallback call shows in cost and traces.
- **D13 v0 data.** Fresh start. v0's database is archived read-only; Langfuse datasets and golden sets carry over because they live outside quack.
- **D14 Hooks.** Hooks are a central investment of v1. Core offers an approval hook (verdicts on gated actions) and a decision hook: by default the decision is made by the same model in the same session, and it can be routed to a dedicated decision model such as Clef or Jev.
- **D15 Decisions vs approvals.** Two levels. A decision is the low-level primitive any extension can raise anywhere (e.g. Sleeper generation choosing an option). An approval is the gate on an action; its approver (e.g. a judge extension) may raise several decisions to reach a verdict.
- **D16 Resolvers.** A decision point runs its resolvers as a declared chain (e.g. deterministic check, decision model, judge, human); each answers or escalates, for example on low confidence.
- **D17 Hook points.** Versioned v1 hook contracts: model request/response, tool before/after, yield (a checker may continue the run with feedback), and session lifecycle (created, resumed, idle, closed, compaction, trigger arrival).
- **D18 Default decider.** The default resolver is the session's own model on a forked turn with full session context and the warm cache, answering against a schema; the fork is discarded so the main transcript stays clean.
- **D19 Session scope.** A session is one unit of work. Triggers map to a stable session key (e.g. `github:owner/repo#123`), so all runs on that work share one log and workspace; ad-hoc UI chats are their own sessions.
- **D20 Sign-in.** Humans sign in through an external OIDC provider; quack stores no passwords.
- **D21 Schedules.** Durable schedules are an extension that submits to sessions; core knows only submissions.
- **D22 Notifications.** The web UI, installed as a PWA, sends web push when something needs the operator (approval waiting, report ready, session parked).
- **D23 CLI.** The CLI does operations and scripting: manage sessions, extensions, agents and evals, with pipe-friendly JSON output. No chat TUI.
- **D24 Retention.** Each agent manifest declares how long idle sessions keep their log and workspace.
- **D25 Egress.** Deny-by-default egress with a per-agent allowlist is enforced in the home deployment too.
- **D26 Gate split** (refines D3). The harness owns the mechanism: declared gated actions wait durably, the approver sees the full action, a rejection returns to the model, and every step is audited. Extensions own the policy: who approves, roles, escalation chains, the judge.
- **D27 Extension shipping** (was O2). Extension code is versioned and released on its own, and agent manifests pin exact versions; an upgrade is a manifest bump plus a benchmark that redeploys only that agent. Prompts, skills and rubrics are versioned separately and change without a deploy, still pinned per agent and benchmarked. New sessions start on new pins; running sessions finish on old ones.
- **D28 Model access.** A model module inside the harness owns provider clients, retries, per-agent and per-task model choice, and the opt-in fallback (D12). llm-swap stays a plain OpenAI-compatible endpoint; a separate gateway is an optional host component later.
- **D29 Cost.** Local model usage is priced at cloud-equivalent rates, so local and hosted runs compare directly and budgets mean the same thing everywhere.
- **D30 UI jobs.** The web UI launches with four jobs: start and follow work (watch live, steer, queue, abort); an approvals inbox across sessions; reading deliverables as finished things; operating the platform (agents, pins, evals, cost, parked or failed sessions).
- **D31 Telemetry.** Quack emits OTLP traces and metrics; Langfuse and Prometheus/Grafana are where trends are read. The UI shows per-session cost and status only; no in-app usage dashboard.
- **D32 Execution model** (resolves O3). By default a session is one agent loop in one role (researcher, reviewer, implementer). Plans, meaning a planner splitting work across parallel agents with different jobs, are an opt-in extension reserved for large projects where work truly parallelizes. Workflows (fixed, code-owned steps) are a separate extension feature. Core offers child tasks and subagents as primitives; requirements state outcomes, not decomposition.
- **D33 Role per run.** A session's role can change at the start of a run (when new input arrives), e.g. a PR session runs `/review` as reviewer and `/fix` as implementer over one log and workspace. A role does not change mid-run.
- **D34 Sessions that start sessions.** A session (e.g. an orchestrator) can start other sessions, keep their handles, and later resume, message, steer or stop them; their results come back to it as input. This is the plans extension's mechanism and is also open to any agent.
- **D35 Workflows at launch.** Sleeper weekly jobs and reMarkable ingest run as workflows: fixed steps in code, with a model called only for steps that need judgment. Research and GitHub run as sessions.
- **D36 Code mode.** On by default: the model can script many tool calls and only the script's output enters context. A manifest can turn it off for an agent whose benchmark favors plain tool calls. Calls made from a script pass through every tool hook and approval, so a script cannot bypass the gate.
- **D37 Subagent questions.** A low-friction tool asks a subagent a self-contained question: a child conversation inside the session, optionally on another model, whose answer returns as a tool result and whose cost counts to the session. It is not checked, since nothing leaves the session; the parent's deliverable is. Distinct from D34's long-lived child sessions.
- **D38 Self-approval.** No rule against approving an action your own input caused in v1.
- **D39 Session roles.** Owner (administers the session), driver (sends input and steers; one at a time, handoff allowed), approver (resolves gated actions), observer (watches).
- **D40 Budgets.** Observe only: cost is reported per session and run, with no caps or spend alerts. Turn limits and SLOs bound the work.
- **D41 Approval default** (refines D3, D26). Every external effect is gateable; each agent's manifest declares which effects need a verdict and from whom. An effect with no approval chain configured proceeds without waiting and is still audited.
- **D42 Artifacts** (resolves O1). An artifact is a finished product of a session (research report, PR review, Sleeper card set), never intermediate data. It is a file in the session workspace that an extension declares by path and kind (schema and renderer); agents edit it with ordinary file tools, and there are no artifact-specific tools. Writes to declared paths are validated early by a tool-after hook. Sealing, when the file goes to a checker or is delivered, snapshots it into the session log as an immutable version with its check verdicts, so what was checked is what was sent and artifacts outlive workspace retention. Core indexes sealed artifacts across sessions and renders kinds without a renderer as Markdown or JSON. Publishing (export, send to the tablet, post to GitHub) is an extension's action on a sealed version.
- **D43 Human edits.** Humans do not edit artifacts; they steer the agent to change them.
- **D44 MCP and A2A.** Quack MUST expose its agents over both MCP and A2A, so other agents (e.g. pi) can start checked work, follow it and get the artifact back while the session stays visible and approvable in quack. Agents MUST be able to use external MCP servers as tools, under the agent's egress allowlist.
- **D45 Targets vs diagnostics.** v1 targets are: the D9 speed SLOs; no benchmark regression for any agent on a change; and judge trust (replay agreement at least 95% of rounds and 98% of criteria, no added false passes, false fails or slowdown on golden sets, no rubric criterion at its maximum on more than 90% of real rounds). Cache hit rates, tool rejections, revision counts and cost are Grafana diagnostics, not targets. UI streaming within 300 ms and typing latency under 50 ms stay hard requirements.
- **D46 No synthesis SLO** (amends D9). The default session writes its own artifact, so there is no separate synthesis stage; session SLOs include writing, and the plans extension measures its own assembly step.
- **D47 Overhead and recovery.** Visible activity within 5 seconds of any submission or verdict, for new, idle or resumed sessions, with setup (container boot, clone) streamed as progress; total setup time counts inside each workload's SLO. A session whose process crashed runs again within 60 seconds on the same host. Resuming on another host must work, with no time target until there is a second host.

## Research extension

- **R1 Clarifying.** A decision point judges whether a question is ambiguous enough to ask first; if so the operator gets a few clarifying questions, otherwise research starts and the report states its assumptions.
- **R2 Sources at launch.** Web search and fetch (pages and PDFs), code and repos, academic papers, and the operator's own documents (past reports and reMarkable documents, R7).
- **R3 Failed checks.** When the budget runs out with checks still failing, the report is delivered with each failure marked inline and a "not fully checked" banner.
- **R4 Follow-ups.** The session stays open after delivery. The agent decides each follow-up's shape: by default a direct, checked answer in the session; a follow-up that changes the findings produces a new report version.
- **R5 Planner.** Research runs as a single session in the researcher role (D32); it does not use the planning extension by default.
- **R6 Report outputs.** A reading view in the UI with sources and check status, Markdown/PDF export, and send-to-reMarkable.
- **R7 Own documents.** Past quack reports and reMarkable documents are citable sources at launch.
- **R8 SLO.** A checked deep report meets the 8-minute p90 from D9, including checker time.

## GitHub extension

- **G1 Triggers.** A label starts the first review; comment commands (e.g. `/review`) start later ones. No automatic review on every PR or push.
- **G2 Evidence up front.** Before a review starts, the reviewer gets repo conventions (AGENTS.md/CLAUDE.md, lint config, linked issue) and the blast radius of each changed symbol (callers, implementations, tests that touch it).
- **G3 Shell.** The reviewer keeps unrestricted shell. Speed comes from G2 evidence and the D9 SLO, not from limiting commands.
- **G4 Scope.** PR review, implement from an issue, fix failing CI or review findings on a PR, and answer questions in PR and issue threads.
- **G5 Triggers.** The label starts the first review; `/review` and `/fix` comment commands start later runs; assigning an issue to the App starts implement-from-issue; mentioning the App in a PR or issue comment starts a thread answer.
- **G6 No redelivery sweep.** Missed webhook deliveries are not recovered automatically; `/review` recovers a miss by hand.

## Sleeper extension

- **S1 Jobs kept.** Lineup and waivers, trades (finder, analysis, trade talk with give/get), league digest and reports (reporter, history, player trends), and predictions (Clef stat-line ranges, observe-only until they beat projections).
- **S2 Delivery.** Results appear on the Sleeper page in quack (service half), and any card can open a session to ask follow-ups about it. No push and no posting to the league at launch.
- **S3 Schedule.** Each weekly workflow runs on its schedule and can also be started from the page.
- **S4 League history.** Daily snapshots, past suggestions and their outcomes are kept indefinitely as the extension's cross-session state, used by analysts and to score suggestions and predictions against what happened.
- **S5 SLOs.** Scheduled jobs finish at least 1 hour before the deadline they serve (the first kickoff a lineup covers, waiver processing). On-demand jobs started from the page meet a 3-minute p90.

## reMarkable extension

- **RM1 Ingest output.** A searchable transcript (citable by research, R7), classification and filing by topic and type, and extracted actions (todos, questions) proposed as gated actions such as starting a research session.
- **RM2 Selection.** The operator picks documents in quack; nothing is ingested automatically, because autosave makes half-written notes look new.
- **RM4 No SLO.** Ingest is a batch nobody waits on; the artifact-ready push says when it is done, and time per document is a diagnostic.
- **RM3 Connection.** rmfakecloud is the only source. The extension also uploads documents to the tablet (research reports, R6).

## Memory, judge and eval extensions

- **MEM1 Recall.** Approved memories are scoped (global, per agent, per repo or league). A small set is pinned as a stable prompt section for matching sessions; the rest is searchable through a recall tool.
- **MEM2 Success.** Memory must win a with-and-without benchmark for an agent, or it is turned off for that agent.
- **J1 Revisions.** Each agent's manifest declares how many revise-and-recheck rounds its checker may run before the deliverable goes out with failures marked (R3). The SLO (D9) still applies.
- **EV1 Eval data.** Datasets and experiment results live in Langfuse; quack exports sessions to it and runs experiments against it.
- **EV2 Dataset bar.** Before an agent reaches prod, its benchmark has at least 20 real cases including known-bad ones; repeats and golden labels are added only when a comparison is a close call.

## Schedules extension

- **SC1 Missed runs.** Each schedule declares how long a run stays useful: a deadline or a staleness window, by default until the next occurrence. After downtime, a missed run still within it runs once, with several misses merged into one; a run past it is skipped and recorded as missed in the operate view.
