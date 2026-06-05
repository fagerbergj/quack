# Quack — Milestones

The build plan. Each milestone is an **end-to-end increment** with a clear "done when", not a
layer in isolation. Architecture lives in the project `README.md`;
per-choice config in [configuration.md](configuration.md).

Format per milestone: **Goal · Scope · Done when · Out of scope**.

---

## M0 — End-to-end skeleton

**Goal.** Prove the whole pipe end to end — frontend → API → orchestrator → real LLM → streamed,
persisted response — with the orchestrator **stubbed** (no agent dispatch yet), and everything green
in CI.

**Scope.**

- **Repo**: renamed **`quack`**; `PLAN.md` → `README.md`.
- **CI/CD**: pipeline builds, lints, and runs **unit + integration tests**; **branch protection** on
  (CI must pass to merge).
- **Server**: schema-first **REST + MCP** endpoints over **Streamable HTTP** that invoke the
  orchestrator and stream the event vocabulary. REST exposes the chat / messages endpoint; the MCP
  server exposes an **`ask` tool** that runs the orchestrator and streams the result back.
- **Orchestrator (stub)**: does a **real LLM round-trip via the ADK** and streams the answer.
  **No DAG, no agent dispatch.**
- **Inference**: provider / model factory wired (the `openai` provider).
- **Stores**: connected — the **relational store** persists sessions/messages.
- **Frontend**: simple chat that renders streamed tokens, with **collapsible thinking + tool-call
  blocks** (components ready even though the stub only emits `token`/`done` for now).
- **Local dev/test**: a `Dockerfile` and a `docker-compose` that stand up the app plus a
  **self-contained Postgres**, so the whole stack runs locally with one command for manual testing
  (no external DB needed).
- **Auth**: none (no verification yet).
- **Deploy**: none (production deployment is later) — validated by **unit + integration tests +
  local run**.

**Done when.** Locally via `docker-compose` (app + self-contained DB): open the chat (or call
REST/MCP), send a message, and watch a real model's answer stream back and persist; the same works
through the MCP endpoint; CI is green and branch protection blocks un-tested merges.

**Out of scope (later).** Agent dispatch / DAG, adversarial vetting, memory, A2A, inbound-auth
verification, deployment.

---

## M1 — Config-defined agents + single-agent dispatch

**Goal.** Turn the stub orchestrator into one that **dispatches to a real, config-defined specialist
agent**. Establish the agent-definition mechanism and stand up the **web researcher**. Still
**no DAG** (single dispatch, not a graph) and **no adversarial vetting**.

**Scope.**

- **Agent definition from config**: an agent is an `agent-card.json` + `prompt.md` bundle, with
  config binding its model and an explicit **built-in tool** selection. The tool registry
  (`kind: builtin`) is wired.
- **Web researcher agent**: defined (card + prompt + model + built-in tools: `web_search`, `fetch`,
  `summarize`). `web_search` uses a **keyless / self-hosted** backend (SearXNG on the home server);
  `fetch` is a plain HTTP GET, falling back to a **keyless headless-Chromium render backend**
  (browserless) for JS-rendered pages a bare GET can't read. Both backends are keyless, so M1 needs
  **no outbound credentials**. *(Both are stood up in home-server — internal and keyless:
  `web_search` → `http://searxng:8080/search?q=…&format=json`, render → `POST
  http://browserless:3000/content {"url":…}`. This M1 prerequisite is met.)* `fetch` must guard
  against **SSRF** (reject non-`http(s)`; block private/link-local ranges + `169.254.169.254` after
  DNS and on each redirect hop) since the render backend fetches URLs server-side.
- **A2A dispatch**: agents run as **A2A servers** publishing their `AgentCard`; the orchestrator
  dispatches to an agent as an **A2A client** (ADK `server/adka2a` + `remoteagent`). A2A is the
  orchestrator↔agent protocol from the start (co-located now, promotable to standalone later). The
  agent's activity streams back as real `thinking` / `tool_call` / `tool_result` events. **Single
  agent; no DAG decomposition.**
- No memory, no adversarial vetting; no auth; no deploy (local run + unit + integration tests).

**Done when.** A request to the orchestrator is dispatched **over A2A** to the web-researcher agent,
which uses its built-in tools to produce an answer, with tool calls + thinking streaming to the
chat. Defining a new agent is adding a card + prompt + a config entry.

**Out of scope (later).** DAG decomposition / multi-agent planning, adversarial vetting, memory,
auth, deployment.

---

## M2 — Adversarial vetting + memory

**Goal.** Make a single agent's output **trustworthy**: wrap it in the **trust gate** (self-refine
then an independent judge) and wire **memory** (recall, and commit only *vetted* findings). Still
single-agent; **no DAG** (that is M3).

**Scope.**

- **Self-refine**: a free same-model pre-pass where the worker critiques and revises its own output
  before anything else looks at it.
- **Independent judge**: the adversarial loop (ADK `LoopAgent` + the independent judge, e.g.
  `selene-mini`), bounded by `max_rounds` and a score `threshold`. Output is not trusted until it
  passes (or rounds run out). The **executor** runs this loop around the agent dispatch (call the
  agent, judge the result, re-dispatch to revise on a fail); the judge is a platform-invoked model,
  so agents themselves stay simple.
- **Rubric**: a **standing constitution** of criteria applied to the output. (Per-node,
  planner-written rubrics arrive with the DAG in M3.)
- **Memory**: the `MemoryService` is wired (the **vector store**, which the local `docker-compose`
  gains here); the agent **recalls** via the memory tools and **commits only vetted findings**.
  Goal 5 becomes enforceable now that the judge exists, which is why memory lands here alongside
  adversarial.
- **Surfacing**: the self-refine, judge, and commit activity streams to the UI and over the APIs.
- Single agent; no DAG; no auth; no deploy.

**Done when.** A single agent's answer is self-refined, judged, and only returned and committed to
memory once it passes (or hits `max_rounds`); a later request **recalls** prior vetted findings. The
vetting loop and the memory write/recall are visible in the stream.

**Out of scope (later).** DAG / multi-agent planning, per-node planner rubrics, auth, deployment.

---

## M3 — DAG planning + execution (with visualization)

**Goal.** The orchestrator stops single-dispatching and starts **decomposing a request into a DAG of
agent nodes and executing it**, surfacing the DAG over the APIs and **visualizing it live in the
UI**. Each node carries M2's trust gate, now with a **planner-written per-node rubric**. The driving
use case is **trip planning** ("best time to go to Dublin, and what to do there"), which fans out
into two research nodes and a synthesis.

**Scope.**

- **Planner**: decomposes a request into a **DAG** of agent invocations (nodes) with explicit
  data-dependency **edges**, choosing agents from the card registry, and writes each node's
  **rubric** (acceptance spec). **Budget caps** (max nodes / depth) enforced. For M3 the planner only
  needs to handle the trip-planning shape reliably; general-request robustness is iterative.
- **Executor**: the custom **topological executor** runs the DAG (extends M1's single dispatch to
  many nodes), passing each node's output along its edges to a **synthesizer** node that joins them
  into the answer.
- **Synthesizer agent**: a new **tool-less** agent (capability = model + prompt, no tools) for the
  join node, which also proves the tool-less-agent path. Like any node, its output is vetted.
- **Vetted nodes**: every node runs inside M2's trust gate (self-refine then judge), now scored
  against its **planner-written rubric** rather than only the standing constitution.
- **Event model**: events gain **node scoping** plus DAG/node lifecycle (queued / running / done /
  failed), so activity is attributable to a node and the graph can animate live.
- **APIs (REST + MCP) updated**: the task representation now includes the **DAG** (nodes, edges,
  per-node status); clients can fetch the DAG for a task and stream node-scoped activity + lifecycle.
- **Frontend**: a **DAG view** for a task — the graph of nodes + edges, live-updating status, with
  drill-down into a node's activity (thinking / tool calls).
- No auth, no deploy.

**Done when.** The trip-planning request ("best time to go to Dublin, and what to do there")
decomposes into a DAG (two web-researcher nodes → a synthesizer node), each node runs vetted, and the
synthesizer produces a cited itinerary. You can watch the DAG build and execute live in the UI and
fetch its structure + node states via REST and MCP.

**Out of scope (later).** Auth, deployment.

---

## M4 — Auth + deploy

**Goal.** Take the locally-tested system and make it a **deployed, authenticated service** behind the
gateway. Inbound auth is wired (pluggable OIDC IdP); Quack runs in production on the real stores.

**Scope.**

- **Inbound auth**: the `auth` block (OIDC, pluggable IdP). API / MCP / A2A clients send a **bearer
  token** Quack verifies against the configured issuer + JWKS; behind the gateway's **forward-auth**,
  browser/SPA identity arrives as `trusted_headers`. Caller identity (user, groups) is available for
  authorization, and unauthenticated requests are rejected. The public `openapi.yaml` is exempt.
- **SPA login**: the frontend uses the IdP redirect (Authentik) so the chat is gated.
- **Deploy**: a **repeatable, documented step** (a CD job on merge, or a `make deploy`) that ships
  the container **behind the Traefik + Authentik gateway** (routed at `/api/v1/quack`, registered
  with the central `swagger-ui`), running on the **real Postgres + qdrant**. The pluggable stores
  swap from the local self-contained backends to production via config, with no code change.
- Still **no outbound / delegated (act-as-user) tool auth**.

**Done when.** Quack runs deployed behind the gateway; an authenticated SPA user (via the IdP login)
and a token-bearing MCP/A2A client can both drive it end to end; unauthenticated requests are
rejected; the spec is live in the gateway docs.

**Out of scope (later).** Outbound / delegated tool auth; broader researcher build-out (more
agents/tools, RAG / `rag-researcher`).

---

## Future work (beyond M4)

Everything below is intentionally outside the M0–M4 plan, captured so it is not lost. Most are
"extensible in theory" seams we shaped but did not build.

| Theme | Item | Notes |
| --- | --- | --- |
| Auth | **Outbound tool auth** | Per-tool `auth.kind`: `api_key`, `client_credentials` (OAuth2 M2M), `delegated` (act-as-user). Only inbound OIDC is built. `delegated` needs a per-user token store + consent flow. |
| Inference | **More model providers** | `gemini`, `anthropic` provider `kind`s; only `openai` is implemented. |
| Stores | **More store backends** | `sqlite` (relational), `pgvector` (vector); only Postgres + qdrant are implemented. |
| Tools | **More tool kinds** | `mcp` (consume external MCP servers' tools via ADK `mcptoolset`) and `http` (declarative HTTP tools); only `builtin` is implemented. |
| Vetting | **70B Selene escalation** | Escalate high-stakes / low-confidence nodes to the batched 70B `selene` judge. The CPU `selene-mini` is the single gate for now. |
| Vetting | **Deterministic floor** | A `platform/verify` pass (citation grounding, source provenance, quote fidelity, schema, URL liveness, code/tests) that runs before the judge. Pulled because it forces structured agent output, a bigger decision. |
| Vetting | **Tool-grounded critique** | CRITIC-style critics that call tools to verify claims rather than reason alone. |
| Vetting | **Per-agent adversarial overrides** | Global adversarial policy only for now; later, per-agent judge / threshold / rounds overrides. |
| Agents | **Distributed A2A** | Promote agents from co-located to standalone A2A services (the design is already A2A-ready). |
| Agents | **Orchestrator A2A face** | Expose the orchestrator as an A2A server to external agent clients (M1's A2A is internal orchestrator to agent dispatch). |
| Agents | **Human-in-the-loop confirmation** | Route side-effecting tools through the `confirmation_request` gate. |
| Memory | **Reflexion-style memory** | Store language reflections on failures, not just vetted findings. |
| Planning | **Adaptive re-planning** | Re-plan the DAG as results arrive, versus the chosen static plan-then-execute. |
| Research | **Researcher build-out** | `rag-researcher` + RAG, more agents/tools, and the second example use case ("latest local LLM models for my hardware"). |
