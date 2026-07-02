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

## Phases

Numbers are nominal; **execution follows dependency order: 1 → 3 → 2 → 4 → 5 → tests.**
Phase 3 runs before Phase 2 because (a) Phase 3 is what restores a green build — the
only post-bump failure is the gate's v1.4.0 technique, which Phase 3 replaces; (b) native
HITL (`ResumeOrRequestInput`) lives *inside* the graph Phase 3 creates, so doing Phase 2
first would mean wiring it into the soon-deleted `dag` executor (throwaway).

0. Spec (this doc).
1. **Mechanical bump** — imports + breaking signatures (`session.NewEvent(ctx,…)`,
   unified `agent.Context`, custom-context `IsolationScope()`/`ResumedInput(id)`), genai
   reconcile. Existing architecture, no behavior change.
2. **Native HITL** — replace `get_user_choice` + `pendingChoiceCallID` with
   `ResumeOrRequestInput`.
3. **Graph orchestration** — workers as first-class nodes; refine loop (advisor→worker→judge
   via `RunNode`) inside each; gate as native nodes; delete `internal/dag/` scheduler.
4. **Stream translator** — rebuild to DERIVE our SSE events from v2's structured
   `session.Event` (`NodeInfo` path+RunID, `Output`, `Routes`, `RequestedInput`),
   not from the deleted gate markers. Decision: **keep the frontend-facing SSE
   vocabulary stable** (its abstraction just insulated the frontend through the
   whole v1→v2 bump — don't trade that away by mirroring ADK's internal event
   structs 1:1, which would re-couple wire+frontend to ADK churn). Take the
   "closer to ADK" win on the read side only: `stage`/`round` fall out of node
   `RunID`s (`worker-r0`/`judge-r0`/`worker-r1`); `node_id`/`run_id` from
   `NodeInfo` (drop manual ScopeToRun/ScopeToNode); HITL `RequestedInput` → a
   first-class SSE event. Vocabulary evolves additively (drop `self_refine`; add
   `advisor` later; optionally expose `Routes`/node-state). Also: the gate node
   must `emit` the judge verdict (score/passed/feedback) as a workflow event so
   `node_done` regains its score badge (lost in 3b-2's minimal SSE).
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

## Phase 5 — M8 durability reconcile (decision: nothing to delete)

Goal was to delete run-state persistence that ADK v2 checkpointing now owns.
Investigated; the reconcile is already achieved and there is **no safe further
dedup**:

- The run-state / resume **logic** ADK checkpointing replaces was the *bespoke
  executor* (TopoSort + manual re-execution) — **already deleted in Phase 3b-2**
  (~657 lines). ADK now owns workflow resume via the Postgres **session store**
  (completed first-class nodes are durably skipped on replay).
- The remaining M8 persistence is **client-facing and non-redundant**, with
  distinct lifecycles — deleting either loses a live feature, not redundancy:
  - `DagPlan` / `DagNode` — durable **structured history** (single writer:
    `persistNodeEvent`, folded from the SSE stream), read by `GET chat-detail`.
  - `ChatEvent` — a **windowed replay buffer** for reconnect / `Last-Event-ID`
    (trimmed to `MaxReplay`, cleared at each run start), NOT durable history.

So `DagNode` can't be derived from `ChatEvent` (trimmed) and neither is the
workflow-resume state (that's the session store). No code change — deleting any
of these would regress history or reconnect.

## Phase 2 — HITL clarification (no change needed)

The upfront clarification HITL (`get_user_choice`) lives on the **orchestrator**,
which is a direct `llmagent` **not migrated to the graph** — so the migration
never touched it. The tool + the resume logic (`pendingChoiceCallID` → deliver
the next message as the call's `FunctionResponse`) are intact (M5 PR1 #39). ADK v2
ships **no native `get_user_choice`** to swap the port for (the port already rides
ADK's `LongRunningFunctionTool` primitive), and `workflow.ResumeOrRequestInput` is
for *mid-node* HITL — a research node pausing to ask the user — which is new
capability we don't currently need (YAGNI). No change.

## Phase 3c — per-node steer/cancel (blocked on ADK; whole-run cancel works)

Whole-run cancel works today (`CancelChatStream` cancels the run context). *Per-
node* steer/cancel (M5b) — interrupt one running node, continue the rest, or re-run
it with guidance — is stubbed (`orchestrator.CancelNode`/`SteerNode` return false).
Reimplementing it on the graph needs each node's worker to run under its **own
cancellable context** so cancelling one node doesn't kill the run. ADK v2 `RunNode`
honors *parent* ctx cancellation but exposes **no per-node / sub-branch cancellation
hook**, and `agent.Context` is an interface constructed internally — so per-node
cancel needs either upstream ADK support or fragile internal-context wrapping. A
dedicated follow-up, not a mechanical un-stub.
