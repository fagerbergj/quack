# Spec: migrate orchestration to ADK 2.0

**Goal:** move quack's orchestration onto ADK 2.0's graph/dynamic workflow engine,
delete the bespoke code ADK now owns, and keep the trust gate as the differentiator —
re-expressed as native workflow nodes.

Grounding: the risks are de-risked by a working spike; see
[`.quack/adk2-spike-findings.md`](../.quack/adk2-spike-findings.md) for evidence and the
verified v2 API. Branch: `feat/adk2-migration` (off `main`).

## Scope

**In:** bump `google.golang.org/adk` → `/adk/v2`; delete `internal/dag/` scheduler
(TopoSort + semaphore + per-node runner); re-express orchestration as an ADK graph
(gated worker = first-class node, body = dynamic refine loop); native HITL via
`ResumeOrRequestInput`; update the stream translator for the v2 event schema; reconcile
M8 durability; productionize the spike as integration tests.

**Out (this migration):** changing the SSE wire vocabulary or the frontend contract;
changing agent bundles / prompts / rubrics; adding new agents; the doc/code-review
subsystems (still paused); replacing the OpenAI provider (v2 has none — we keep ours).

## Forbidden

- Do not change `openapi.yaml` or generated files as part of this (unrelated).
- Do not weaken the trust gate: independent judge, weakest-link scoring, citation/length
  checks, and threshold semantics must survive unchanged in behavior.
- Do not break the SSE event names/shapes the frontend consumes (`internal/stream` +
  `frontend/src/state/agentStream.ts` must stay in sync).
- No partial module state on `main`: the bump is all-or-nothing per the module path;
  land it as a coherent branch that builds + tests green.

## Interfaces we depend on (v2, verified)

`workflowagent.New` / `workflow.{New,Chain,NewEdgeBuilder}` · `NewDynamicNode` + `RunNode` ·
`NewAgentNode` · `NewFunctionNode` / `NewEmittingFunctionNode` · `ResumeOrRequestInput` +
`NewRequestInputEvent` + `WorkflowInputFunctionCallName` · `session/database` (durable) ·
`runner.{New,Run}` · `model.LLM` (unchanged; our `openaimodel` adapter kept).

## Output contract (unchanged externally)

Same REST/MCP/A2A surface and same SSE event sequence
(`dag_plan → node_* → agent_* → done`). Internally, node identity comes from ADK's
`session.Event.NodeInfo` instead of hand-scoped run/node IDs.

## Phases (each builds + tests green before the next)

0. Spec (this doc).
1. **Mechanical bump** — imports + breaking signatures (`session.NewEvent(ctx,…)`,
   unified `agent.Context`, custom-context `IsolationScope()`/`ResumedInput(id)`), genai
   reconcile. Existing architecture, no behavior change.
2. **Native HITL** — replace `get_user_choice` + `pendingChoiceCallID` with
   `ResumeOrRequestInput`.
3. **Graph orchestration** — workers as first-class nodes; refine loop (advisor→worker→judge
   via `RunNode`) inside each; gate as native nodes; delete `internal/dag/` scheduler.
4. **Stream translator** — v2 `session.Event` (`NodeInfo`/`Output`/`Routes`/`RequestedInput`).
5. **M8 reconcile** — delete run-state durability now owned by ADK; keep client-facing
   SSE `Last-Event-ID` replay.

## Test strategy (productionized from the spike)

Deterministic, offline, stdlib + sqlite session store (see `go-testing` skill):

1. **Refine-loop convergence** — worker→judge revises until score ≥ threshold in N rounds.
2. **HITL pause→resume** — workflow pauses at `request_input`, resumes with the reply.
3. **Restart durability** — completed first-class worker nodes are NOT re-run after a fresh
   session-service/runner over the same DB (guards the ADK behavior our architecture relies on;
   uses a per-role call-counting stub `model.LLM`).
4. **ADK-behavior contract** — dynamic `RunNode` children re-run vs first-class nodes skip;
   fails loudly if a future ADK bump changes it.
5. **OpenAI provider contract (static/offline)** — httptest-stubbed OpenAI endpoint (no live
   server) asserts `openaimodel` satisfies v2 `model.LLM` and translates correctly:
   non-streaming + streaming aggregation, tool-calls, `reasoning_content`→Thought, usage.

Behavioral drift from this spec becomes a failing test, not a production incident.
