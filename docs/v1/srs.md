# Quack v1 SRS

Status: draft for review, 2026-10-30. This is the normative requirements set for quack's core. The product case is in the [PRD](prd.md); each extension's requirements are in [extensions/](extensions/). Requirement levels follow RFC 2119. The trace column cites [decisions.md](decisions.md) (`D*`) or the operator's platform requirements (`P`), which this document adapts to a single operator (D1).

Areas follow the layers in [cloud-agent-layers.svg](cloud-agent-layers.svg). The SRS states what core must do; it does not choose how. Process topology, language and storage engines are for the architecture RFC.

## 1. Glossary

| Term | Meaning |
| --- | --- |
| Operator | The person who runs this deployment (D1). |
| Participant | Anyone or anything that acts in a session: an authenticated human, or a non-human identity such as a trigger, a checker or another session (D8). |
| Role | What a human participant may do in a session: owner, driver, approver or observer (D39). Separate from an agent role. |
| Agent | A deployable definition: a manifest naming its model, agent role, pinned extensions and knowledge versions, egress allowlist, gated actions, limits and retention. |
| Agent role | The job a run performs, such as researcher, reviewer or implementer. It sets the prompt and tools for that run (D32, D33). |
| Session | One unit of work, identified by a stable session key (e.g. `github:owner/repo#123`). It owns its log and workspace (D19). |
| Log | The session's append-only, durable record of entries and state changes. |
| Run | The work from one input to its final answer. Agent role is fixed for a run (D33). |
| Turn | One model response and its tool calls. |
| Trigger | A non-human source of input: an HTTP call, a webhook, a schedule, another session. |
| Extension | A named, versioned unit that adds behaviour without changing core. It has a session half (tools, hooks, prompt sections, durable state) and may have a service half (always-on ingress, pages, cross-session state) (A2). |
| Knowledge | Prompts, skills and rubrics. Versioned separately from extension code (D27). |
| Hook | An extension callback at a defined lifecycle point (D17). |
| Gated action | A call with an external effect that must receive a verdict before it runs (D3, D26). |
| Approval | The verdict on a gated action: approve, or reject with a reason. |
| Decision point | A question with fixed options that something in a session needs answered (D15). |
| Resolver | Something that answers a decision point or an approval: deterministic code, a model, a checker extension or a human (D16). |
| Checker | A resolver that evaluates an artifact against criteria, such as the judge (D3). |
| Artifact | A finished product of a session (a report, a review, a set of cards): a workspace file an extension declares by path and kind, sealed into the log as immutable versions. Never intermediate data (D42). |

## 2. Functional requirements

### 2.1 Surfaces and triggers

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| SUR-1 | MUST | Accept work over HTTP (`POST /invocations`, JSON or SSE response) and over a bidirectional live channel (`/ws`). | P |
| SUR-2 | MUST | Let a trigger start or continue a session without a UI, acting under its own identity. | P, D8 |
| SUR-3 | MUST | Accept webhooks through an extension's service half, verify each sender, and map each to an agent and session key. | P, A2 |
| SUR-4 | MUST | Let an extension's service half affect sessions only by submitting to a session key through the same path as any trigger. | A2 |
| SUR-5 | MUST | Expose agents over MCP and A2A: start work, follow it, and receive the sealed artifact, with the session visible and approvable in quack. | P, D44 |
| SUR-6 | MUST | Let agents use external MCP servers as tools, declared in the manifest and under the agent's egress allowlist. | D44 |

### 2.2 Dispatch

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| DSP-1 | MUST | Map each request to an agent and a session key. The extension or caller that submits supplies the key. | P, D19 |
| DSP-2 | MUST | Create, resume and stop sessions on demand, including waking an idle session when input or a verdict arrives for it. | P |
| DSP-3 | MUST | Route every request for a session to the one process that owns its storage. | P |
| DSP-4 | MUST | Hold no domain logic. | A2 |

### 2.3 Deployment

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| DEP-1 | MUST | Deploy an agent from one versioned artifact and one manifest declaring its model, agent roles, pinned extension and knowledge versions, egress allowlist, gated actions, turn limit, checker revision limit and retention. | P, D24, D27, J1 |
| DEP-2 | MUST | Let one agent be changed and redeployed without redeploying any other. | P |
| DEP-3 | MUST | Start new sessions on an agent's new pins while running sessions finish on the versions they started with. | D27 |
| DEP-4 | MUST | Give each agent its own identity and no ambient credentials. | P |

### 2.4 Runtime

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| RUN-1 | MUST | Give each session an isolated environment with a shell and developer tooling (git, language toolchains, build tools, test runners). | P |
| RUN-2 | MUST | Give each session a workspace that survives stop and resume and is deleted with the session or at the end of its agent's retention period. | P, D24 |
| RUN-3 | MUST | Implement the container contract: port 8080, `GET /ping` (`HealthyBusy` while work runs, otherwise `Healthy`), `POST /invocations`, `/ws`. | P |
| RUN-4 | MUST | Enforce command timeouts and abort a command's whole process tree. | P |

### 2.5 Harness

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| HAR-1 | MUST | Read and write workspace files, and clone, fetch, commit and push to remote repositories. Pushes are gateable. | P, D3 |
| HAR-2 | MUST | Commit every input, model response, tool call and state change to the log before anyone sees it or the agent acts on it. | P |
| HAR-3 | MUST | Resume after a crash or redeploy without repeating completed work, re-running an interrupted tool call only if it is declared replay-safe. | P |
| HAR-4 | MUST | Resume an idle session with its full history and workspace at any time within its retention period. | P, D24 |
| HAR-5 | MUST | Treat a retried submission with the same request id as the original. | P |
| HAR-6 | MUST | Let several participants share a session with roles: owner, driver, approver, observer. One driver at a time; driver handoff is recorded. | P, D8, D39 |
| HAR-7 | MUST | Show every participant, live: streamed output, tool calls, pending decisions and approvals, the current driver, and who is present. | P |
| HAR-8 | MUST | Let the driver steer a running run, queue follow-ups and abort, leaving the session consistent. | P |
| HAR-9 | MUST | Compact context before it reaches the model's context window, keeping the originals in the log. | P |
| HAR-10 | MUST | Enforce a per-run turn limit, per-tool output and time limits, and stop work that keeps crashing its process, showing it as parked with a retry action. | P |
| HAR-11 | MUST | Let a run take the agent role its input asks for, and keep that role until the run ends. | D33 |
| HAR-12 | MUST | Offer a subagent question tool: a child conversation in the same session, optionally on another model, whose answer returns as a tool result and whose cost counts to the session. | D37 |
| HAR-13 | MUST | Let a session start other sessions, keep their handles, and later message, steer, resume or stop them, receiving their results as input. | D34 |
| HAR-14 | MUST | Offer code mode by default: the model may write a script that calls tools, and only its output enters context. A manifest may turn it off. Every call from a script passes through the same hooks and gates as a direct call. | D36 |
| HAR-15 | MUST | Offer child tasks that wait durably on other tasks, so extensions can build workflows and plans without core knowing either. | D32 |

### 2.6 Extensions and hooks

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| EXT-1 | MUST | Let implementers add tools, prompt sections, hooks, durable state and a service half as named, versioned extensions, without modifying core. | P, A2 |
| EXT-2 | MUST | Provide versioned hook points: model request (edit context) and response (inspect, redact); tool before (block, rewrite, gate) and after (replace, trim); yield (when a run wants to end, continue it with feedback); session lifecycle (created, resumed, idle, closed, compaction, trigger arrival). | D17 |
| EXT-3 | MUST | Load knowledge as prompt sections or as skills the model loads on demand. | P |
| EXT-4 | MUST | Version knowledge separately from extension code, so a prompt, skill or rubric changes without a deploy, still pinned per agent. | P, D27 |
| EXT-5 | MUST | Contain an extension's failure to the session that loaded it. | A1 |
| EXT-6 | MAY | Fetch knowledge from an external prompt manager such as Langfuse. | P |

### 2.7 Decisions and approvals

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| GATE-1 | MUST | Let core and extensions raise a decision point (question, options, evidence, answer schema) anywhere in a session. | D14, D15 |
| GATE-2 | MUST | Resolve a decision point through a declared chain of resolvers, each of which answers or escalates. | D16 |
| GATE-3 | MUST | Default the resolver to the session's own model on a forked turn with full session context, discarding the fork afterwards. | D18 |
| GATE-4 | MUST | Let a manifest route a decision point to another resolver, including a dedicated decision model (e.g. Clef) or a human. | D14 |
| GATE-5 | MUST | Make every external effect gateable, and let each agent's manifest declare which effects need a verdict. An effect with no approval chain proceeds without waiting and is audited. | P, D3, D41 |
| GATE-6 | MUST | Make a gated call wait durably, across restarts and for days, until a resolver approves it or rejects it with a reason. | P, D26 |
| GATE-7 | MUST | Show an approver the full action (target, payload, acting identity) and the evidence and checks behind it. | P, D26 |
| GATE-8 | MUST | Return a rejection and its reason to the model. | P |
| GATE-9 | MUST | Leave who approves, escalation and roles to extensions; core holds no approval policy, rubric or checker logic. | D26 |

### 2.8 Artifacts

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| ART-1 | MUST | Let an extension declare artifact kinds by workspace path, schema and renderer. Agents create and edit artifacts with the ordinary file tools. | D42 |
| ART-2 | MUST | Validate writes to declared artifact paths and report every schema problem at once. | D42 |
| ART-3 | MUST | Seal an artifact when it goes to a checker or is delivered: snapshot it into the session log as an immutable version, and attach every check verdict to that version. | D42, UI-7 |
| ART-4 | MUST | Deliver a sealed version by reference, so the delivered content is exactly the checked content. | D42 |
| ART-5 | MUST | Keep sealed versions after the workspace is deleted, and index them across sessions by kind, session and time. | D42, R7, D10 |
| ART-6 | MUST | Render kinds without a renderer as Markdown or JSON. Humans do not edit artifacts; they steer. | D42, D43 |

### 2.9 Model access

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| MOD-1 | MUST | Choose the model per agent, agent role, conversation, subagent and decision point, across providers, by configuration, effective from the next request. | P, D28 |
| MOD-2 | MUST | Support several OpenAI-compatible and native providers; llm-swap is one such endpoint. | P, D28 |
| MOD-3 | MUST | Retry failed model requests with backoff; during an outage keep sessions readable and queued input waiting. | P |
| MOD-4 | MAY | Let an agent opt in to a fallback model on another provider. Off by default; every fallback call is marked in cost and traces. | P, D12 |

### 2.10 Identity and governance

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| GOV-1 | MUST | Sign humans in through an external OIDC provider. Quack stores no passwords. | D20 |
| GOV-2 | MUST | Attribute every input to an authenticated human or a trigger's identity. | P |
| GOV-3 | MUST | Keep an append-only audit record of inputs, driver handoffs, decisions, approvals, and gated or external calls with their target, acting identity and outcome. | P |
| GOV-4 | SHOULD | Make the audit record queryable by session, agent, participant and time. | P |

### 2.11 Observability and evaluation

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| OBS-1 | MUST | Emit OpenTelemetry traces over OTLP for submissions, task phases, model requests (provider, model, tokens, cost), tool calls, decisions and approvals. | P, D31 |
| OBS-2 | MUST | Price local model usage at cloud-equivalent rates, and attribute cost per session, run, agent and model. | P, D29 |
| OBS-3 | MUST | Show live cost and status to the session's participants. | P, D31 |
| OBS-4 | MUST | Record every run so it can be replayed against another model, prompt or extension version. | D10 |
| OBS-5 | MUST | Block an agent's change (model, extension pin, knowledge pin) from reaching prod until its benchmark has run on the candidate. | D10, D27 |
| OBS-6 | SHOULD | Continue a trace across a restart. | P |

### 2.12 Web UI

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| UI-1 | MUST | Start and follow work: start a session, watch it live, steer, queue, abort. | D30 |
| UI-2 | MUST | Provide one approvals inbox across sessions. | D30 |
| UI-3 | MUST | List artifacts across sessions and show each as a finished, readable thing with its versions and check status, separate from the transcript. | D30, D42 |
| UI-4 | MUST | Let the operator see agents and pins, run and compare benchmarks, see cost, and list, inspect, retry, abort and delete sessions. | D30, P |
| UI-5 | MUST | Host extension pages (e.g. the Sleeper page) in the same shell and visual language. | S2 |
| UI-6 | MUST | Install as a PWA and send web push when an approval waits, an artifact is ready, or a session is parked. | D22 |
| UI-7 | MUST | Answer "why N rounds, which check failed, what it cost, which calls were rejected" within one screen of a session. | D30 |

### 2.13 CLI

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| CLI-1 | MUST | Manage sessions, agents, extensions, knowledge pins and evals from the command line, with JSON output for scripts. | D23 |
| CLI-2 | MUST | Cover every operation the UI's operate job offers. | D23, D30 |

## 3. Non-functional requirements

### 3.1 Performance

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-PERF-1 | MUST | p90 session time, including checker time: code review 10 min, research report 8, one checker round 3. | D9, D46 |
| NFR-PERF-2 | MUST | Keep prompts ordered and stable, with a stable provider session identity per conversation, so prompt caches stay warm. | P |
| NFR-PERF-3 | MUST | Show visible activity within 5 s of any submission or verdict, whether the session is new, idle or resumed, streaming setup steps (container boot, clone) as progress. Total setup time counts within each workload's SLO. | P, D47 |

### 3.2 Durability and recovery

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-HAR-1 | MUST | A process crash loses no committed state, and no more streamed partial output than a configurable window. | P |
| NFR-HAR-2 | MUST | A session whose process crashed runs again within 60 s on the same host. A session can resume on another host from its storage; no time target until there is a second host. | P, D4, D47 |
| NFR-HAR-3 | MUST | Scale by session: one process per session, nothing shared between sessions. | P |

### 3.3 Isolation and security

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-SEC-1 | MUST | Sessions share no kernel, memory, filesystem or credentials. Agent processes run as non-root. | P |
| NFR-SEC-2 | MUST | Egress is deny-by-default, with a reviewed per-agent allowlist, enforced in the home deployment. | P, D25 |
| NFR-SEC-3 | MUST | Credentials are injected at run time, short-lived and scoped, and never baked into images or written to disk. | P |
| NFR-SEC-4 | MUST | Gated actions, egress allowlists, permitted models and limits are configured per agent and reviewable as code. | P |
| NFR-SEC-5 | MUST | Session state is encrypted at rest and in transit and deletable on request. | P |
| NFR-SEC-6 | MUST | Trace attributes carry identifiers and metadata only by default; prompt and output content is opt-in. | P |

### 3.4 Portability and scale

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-PORT-1 | MUST | The session runtime runs unchanged on any host implementing the container contract (local Docker, ECS, AgentCore). | P, D1 |
| NFR-PORT-2 | MUST | v1 runs on one home server; adding hosts needs no agent or extension change. | D4 |
| NFR-PORT-3 | MUST | Idle sessions release compute after a configurable timeout. | P |

### 3.5 Compatibility

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-COMP-1 | MUST | Extension and hook contracts are versioned. A minor release never breaks extensions built for the previous one, and contracts are deprecated before removal. | P, D17 |
| NFR-COMP-2 | MUST | Adding a model provider changes no agent code. | P |
| NFR-COMP-3 | MUST | The core version is pinned per agent and benchmarked before and after each upgrade. | P, D10 |

### 3.6 Operations

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-OPS-1 | MUST | Emit metrics for latency, tokens, cost, errors and active sessions, and alert on failed sessions, crash loops and model error rates. | P, D31 |

### 3.7 Accessibility and UI quality

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| NFR-UI-1 | MUST | Meet WCAG 2.2 AA in light and dark themes, at desktop and phone widths. | D6 |
| NFR-UI-2 | MUST | Follow the design-frontend skill (`~/.agents/skills/design-frontend`): semantic tokens, one accent, native elements first. | D6 |
| NFR-UI-3 | MUST | Show streamed output within 300 ms of the server emitting it, and keep composer input latency under 50 ms in a 200-turn session while a run streams. | D45 |

## 4. Out of scope for v1

Teams and per-team cost attribution; delegated user identity; budget caps and spend alerts (D40); the self-approval restriction (D38); an in-app usage dashboard (D31); a chat TUI (D23); migrating v0 data (D13).

## 5. Open items

None at the core level. Extension documents list their own.
