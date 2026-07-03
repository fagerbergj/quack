# Quack — Milestones

The build plan. Each milestone is an **end-to-end increment** with a clear "done when", not a
layer in isolation. Architecture lives in the project `README.md`;
per-choice config in [configuration.md](configuration.md).

Format per milestone: **Goal · Scope · Done when · Out of scope**.

## M0 — End-to-end skeleton ✅

<details>
<summary><strong>✅ Complete.</strong> The whole pipe runs via <code>docker compose up</code>: chat streams
<code>thinking → tool_call → tool_result → token → done</code> from a real model and persists across an
app restart; the MCP <code>ask</code> tool works; CI is green and <code>main</code> is protected.</summary>

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
  blocks** — live: a thinking model + a trivial `current_time` tool make the stub emit real
  `thinking` / `tool_call` / `tool_result` / `token` events.
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

</details>

## M1 — Config-defined agents + single-agent dispatch ✅

<details>
<summary><strong>✅ Complete.</strong> The orchestrator is now a thin LLM dispatcher that delegates
<strong>over A2A</strong> to a config-defined <code>web-researcher</code> agent (card + prompt +
built-in <code>web_search</code> / <code>fetch</code> / <code>summarize</code> tools, with an
SSRF-guarded fetch); the agent's <code>thinking</code> / <code>tool_call</code> /
<code>tool_result</code> activity streams back to the chat. Defining a new agent is a card + prompt +
config entry. Builds green; unit tests for the agent bundle, A2A wiring, and tool/SSRF layers pass.</summary>

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

</details>

## M2 — Adversarial vetting ✅

<details>
<summary><strong>✅ Complete.</strong> A single agent's output is self-refined and independently judged against a standing rubric; the vetting loop streams to the UI and APIs.</summary>

**Goal.** Make a single agent's output **trustworthy**: wrap it in the **trust gate** (self-refine
then an independent judge). Still single-agent; **no DAG** (that is M3); **no memory** (that is M3).

**Scope.**

- **Self-refine**: a free same-model pre-pass where the worker critiques and revises its own output
  before anything else looks at it.
- **Independent judge**: the adversarial loop (ADK `LoopAgent` + the independent judge, e.g.
  `gemma4-26b-a4b`), bounded by `max_rounds` and a score `threshold`. Output is not trusted until it
  passes (or rounds run out). The **executor** runs this loop around the agent dispatch (call the
  agent, judge the result, re-dispatch to revise on a fail); the judge is a platform-invoked model,
  so agents themselves stay simple.
- **Rubric**: a **standing constitution** of criteria applied to the output. (Per-node,
  planner-written rubrics arrive with the DAG in M3.)
- **Surfacing**: the self-refine and judge activity streams to the UI and over the APIs.
- Single agent; no DAG; no memory; no auth; no deploy.

**Done when.** A single agent's answer is self-refined, judged, and only returned once it passes (or
hits `max_rounds`). The vetting loop is visible in the stream.

**Out of scope (later).** Memory (M6), DAG / multi-agent planning (M3), per-node planner rubrics, auth, deployment.

</details>

## M3 — DAG planning + execution (with visualization) ✅

<details>
<summary><strong>✅ Complete.</strong> The orchestrator decomposes a request into a <strong>DAG</strong>
of agent nodes and runs it topologically — each node wrapped in M2's trust gate — with a tool-less
<code>synthesizer</code> joining the results. The DAG streams over the APIs (node-lifecycle events) and
animates live in the UI (<code>DagView</code> / <code>DagNode</code>). <em>Inferred complete from the
code; confirm the Dublin trip-planning demo is validated.</em></summary>

> **Engine replaced (2026-07).** The custom topological executor described below was replaced by
> the **ADK v2 graph engine** (PRs #117–#118): orchestration is now a single ADK workflow/runner
> (plan node → execute node → graph run in `internal/dag/graph.go` / `rundag.go`), with the trust
> gate and node lifecycle native to it. The milestone's *behavior* (decompose → vetted nodes →
> synthesizer, live DAG view) is unchanged; this body is the historical implementation.

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
- No memory, no auth, no deploy.

**Done when.** The trip-planning request ("best time to go to Dublin, and what to do there")
decomposes into a DAG (two web-researcher nodes → a synthesizer node), each node runs vetted, and the
synthesizer produces a cited itinerary. You can watch the DAG build and execute live in the UI and
fetch its structure + node states via REST and MCP.

**Out of scope (later).** Memory (M6), auth, deployment.

</details>

## M4 — Multi-modal input (photo + audio) ✅

<details>
<summary><strong>✅ Complete.</strong> Image and audio attachments flow end to end as A2A-native
multi-modal parts: a photo routes to <code>media-reader</code>, a handwritten note to the
<code>image-reader</code> specialist (<code>qwen3-vl-32b</code>), and an audio clip transcribes via
<code>media-reader</code> — each producing understanding text in one turn that persists in history for a
follow-up turn to act on. Native <code>image_url</code> / <code>input_audio</code> throughout; cleaned
output renders with the raw transcript in a collapsible <code>&lt;details&gt;</code> block.</summary>

**Goal.** Accept **image and audio input** end to end and route it to the right media agent — a general **`media-reader`** or
the **`image-reader`** specialist — that consumes the payload via **native multi-modal parts** (OpenAI `image_url` / `input_audio`, carried as
**A2A's native message parts**) — producing **understanding (text) in a single turn**. Acting on what
the media *contains* is handled across **two turns** for now (the understanding lands in history; a
follow-up turn acts on it); a content-dependent DAG is **M12**.

**Scope.**

- **Ingress**: a **multipart** message goes to the **orchestrator**, which stores the media in a
  **pluggable blob store** (local filesystem now, S3-style later) and represents the user message as
  **native multi-modal parts** (`genai.Part` `InlineData` / `FileData`), persisted with the turn so
  revise loops + follow-ups can re-thread them. (`openapi.yaml` is the source of truth for the body.)
- **A2A-native threading**: adopt **A2A's native multi-modal Message / Part** representation wherever
  content flows (orchestrator ↔ agent, persistence, history) — *apply it everywhere*, no custom
  sidecar. The **text-only planner** (`qwen3.6`) routes on attachment **metadata** (it can't consume
  image / audio bytes); the bytes reach only the chosen media agent's model. The text-only drop
  points (`buildHistory`, `questionText`, the revise builders, `buildTask`) are fixed to **preserve
  non-text parts**.
- **Inference**: finish the `openaimodel` adapter's **native** mapping — `image_url` (already works) +
  **`input_audio`** (new); the audio hard-error (`internal/inference/openaimodel/openai.go:442`) is
  replaced. **Video / PDF stay rejected** (deferred).
- **Models / llm-swap**: two new **co-resident worker+judge groups** mirroring `chat` — a **`media`
  group** = **`Qwen3-Omni-30B-A3B-Instruct`** (an *omni* LLM: image + audio + text in; MoE 30B / **3B
  active**, fast) **+ gemma**, and a **`vision` group** = **`Qwen3-VL-32B-Instruct`** (the dedicated
  vision model for *hard* image reading — handwriting) **+ gemma**. Both `exclusive` (they evict
  `chat`; media stages run sequentially, so a worker swap is fine). Because **gemma is a shared member
  of `chat` + `media` + `vision`, it stays warm** — only the `coding` group evicts it (gemma can't be
  `persistent`: the ~49 GB `qwen3-coder-next` needs the whole box). Both come from **official
  `ggml-org` / Qwen** GGUF repos (`-hf` auto-pulls the projectors); Qwen3-Omni **Q4_K_M 18.6 GB**,
  qwen3-vl ~20 GB — each + gemma fits, and they swap on demand (a request uses one or the other).
- **Why an omni-LLM, not a dedicated ASR**: pure-ASR models (`Qwen3-ASR-1.7B`, `Granite-speech-2b`) are
  ~2 WER points *more* accurate **and** have official GGUFs — but they **aren't steerable**: they can't
  follow "mark `[inaudible]`, label speakers, don't translate" and can't **revise on judge feedback**,
  so the M2 vetting loop (draft → judge → revise) breaks — the judge would be useless. A steerable
  omni-LLM keeps vetting working, at a modest WER cost. (A dedicated ASR can still be added later as a
  *consensus tool* for fidelity — see the judge/rubric design.)
- **Build caveat**: Qwen3-Omni audio support is recent — smoke-test it loads + transcribes on ROCm;
  confirm the qwen3-vl projector loads too (you already run it, so low risk).
- **Judge fidelity caveat**: gemma runs `--no-mmproj` (text-only), so it vets the **output text's**
  quality / format, **not fidelity to the source** image/audio. Source-fidelity vetting needs a judge
  that sees / hears the source — gemma-4 *is* natively multimodal, so re-enabling its projector is a
  **future** option; for now the worker's self-refine covers fidelity.
- **Two media agents, card-routed**: a general **`media-reader`** (`Qwen3-Omni` — image + audio +
  reasoning) and a vision specialist **`image-reader`** (`Qwen3-VL-32B` — *difficult* image reading:
  handwriting, dense OCR), each with its **own rubric**. The **`image-reader` card's when-to-use**
  explicitly marks it for hard image tasks, so the **orchestrator routes by card description + the
  request text alongside the image** ("transcribe this handwritten note" → `image-reader`; "what's in
  this photo" → `media-reader`; any audio → `media-reader`, the only one with ears) — the same
  card-based selection the planner already uses (see M7).
- **Vetting the media agents**: the worker's **self-refine re-listens / re-looks** at the source and
  corrects its own draft — but the **pre-correction draft ("what it heard/saw") is kept alongside** the
  cleaned output, so every LLM correction is **auditable** (guards against the model "correcting" a
  right word to a wrong one; you can always fall back to the raw). The deliverable renders **cleaned /
  reasoned text up top, with the raw verbatim in a collapsible `<details><summary>Original
  Transcript</summary>` block** (reusing quack's existing `<details>` Sources idiom); the raw also
  persists as an artifact. The **deaf/blind judge** (gemma) can't verify
  fidelity, so it scores **plausibility + coherence** ("is this reasonably something a person said /
  does it make sense?") plus instruction-following (cleaned per spec, right language, no boilerplate)
  and hallucination signatures. When the media-reader genuinely can't resolve the audio / image, it
  **flags the uncertainty in its output** (mid-DAG node parking was removed from M5; reviving it would
  let a node ask the human instead); a consensus-ASR fidelity tool is optional future hardening.
- **Capability tags**: providers / agents gain **`text` / `vision` / `audio`** tags so the factory
  routes parts to a capable model and rejects a mismatch at dispatch.
- **Surfacing**: attachments render in the chat UI; the agent's answer streams as usual.

**Done when.** Attach a photo and ask about it → **`media-reader`** answers; attach a photo of a
**handwritten note** and ask to transcribe → the orchestrator routes to the **`image-reader`**
specialist (`qwen3-vl-32b`); attach an audio clip → **`media-reader`** transcribes it. Native
`image_url` / `input_audio` throughout; the result **persists as text in history**, so a follow-up
turn can act on it. The orchestrator does **not** plan a content-dependent DAG on unknown media
(that's M12).

**Out of scope (later).** Content-dependent / adaptive DAG planning on media (**M12**); **video** (a
later group + agent); the document-ingestion pipeline (the paused Document milestone); embedding / indexing.

</details>

## M5 — Human-in-the-loop (clarify) ✅

**Goal.** The orchestrator **clarifies an ambiguous ask before launching a DAG** — agent-driven, not an
automatic confidence gate (the M2 judge keeps its continue-but-warn behavior).

**Scope.**

- **Upfront clarification — no suspend machinery.** Before planning, the orchestrator may ask a
  **clarifying question instead of launching a DAG**, via the long-running **`get_user_choice`** tool
  (a port of Python ADK's; Go ADK lacks a native one). It poses a question + discrete options; the turn
  pauses, the UI surfaces a choice prompt, and the user's reply resumes the orchestrator's **own
  session** as the tool's `FunctionResponse`. The planner then plans with the clarification in history
  (`buildHistory` carries it), asking again if still ambiguous. No parked DAG state, no resume API — the
  orchestrator is the top-level session, so the existing chat loop covers this entirely.
- **Readiness-driven executor (kept).** M3's strict per-layer barrier was replaced by a scheduler that
  runs a node **as soon as its `DependsOn` are all `done`**, so independent branches make progress
  concurrently. This landed alongside M5b; the executor itself was since replaced by the ADK v2
  graph engine (see the M3 note), which preserves the readiness-driven concurrency.

**Done when.** An ambiguous request triggers an **upfront** clarifying question (a choice prompt) before
any DAG runs; the user's answer flows back into the same turn and planning proceeds.

**Removed (was M5b — mid-DAG node pause/resume).** A prototype let a *node* pause mid-DAG on a
long-running `request_input` call (gate pause → `node_waiting` → persisted `waiting` state → a
`resumeDagNode` endpoint that reconstructed the DAG from storage and re-entered `executor.Resume`). It
was **removed**: unlike the orchestrator (the top-level session, trivially resumed on the next chat
reply), a node is a nested sub-session, so resuming it requires reconstructing the whole DAG from
storage — inherent complexity for a flow with no concrete need yet. A throttled tool (e.g. rate-limited
`web_search`) wants backoff/retry inside the tool, not a human pause, so that case doesn't justify the
plumbing either. Recoverable from git history if a real need appears.

> **Update (M5b shipped, 2026-06).** Mid-node steering later shipped **without** reviving this
> machinery: `SteerNode` interrupts the node's context and **re-runs the same session** with the
> guidance appended, so prior tool calls/results are retained (PRs #108–#112, React + TUI parity).
> True mid-node *pause for human input* is a separate effort on the native ADK v2 path
> (`feat/node-hitl`).

**Out of scope (later).** `RequireConfirmation` approve/deny gate for side-effecting tools (folds into
the paused Code-review milestone's GitHub writes — though M9's `bash` safety gate is a related
approve/deny path); the `doc-ingest` / `code-review` skills; automatic confidence-based parking.

## M6 — Memory (explicit recall + gated, consolidating commit) ✅

<details>
<summary><strong>✅ Complete.</strong> Embedder, Qdrant-backed recall, the gated/consolidating
task-memory write path, the optional <code>memory.md</code> bundle guidance, and config-gated user
memory are merged. Model-driven memory ops (<code>stage_memory</code> / <code>commit_memory</code> /
<code>load_memory</code>) already surface as tool activity in the UI; dedicated events for the two
silent platform paths (ambient <code>preload_memory</code>, the gate's background commit) are deferred
as future polish. The design diverged from the original M6 sketch during planning — recorded
as-built below.</summary>

**Goal.** Give agents **durable, semantic memory**: recall prior knowledge and commit new knowledge.
Memory is **explicit** (deliberate writes, never background auto-capture) and **semantic only** —
*episodic* memory is the existing ADK session/Postgres history and *procedural* memory is Skills (M7).
Two kinds, by role:

- **Task memory** — research **tradecraft** (which sources are authoritative and for what, which are
  junk, search/fetch tactics that work, availability dead-ends), owned by **web-researcher**. *Not*
  world-facts — those go stale and must be re-fetched to cite. **Gated**: the agent stages, the judge
  pass commits.
- **User memory** — facts *about the user* (identity, preferences, relationships, possessions, goals,
  limits), owned by the **orchestrator**. **Ungated** (grounded in what the user said) but **off by
  default** (`memory.user_memory`, a privacy choice).

**Scope (as built).**

- **Qdrant-backed `memory.Service`.** Implements ADK's `memory.Service` (`SearchMemory` live;
  `AddSessionToMemory` a deliberate no-op — Quack never auto-ingests). A new keyless `qdrant`
  `docker-compose` service. Two collections (`task_memory`, `user_memory`), **per-user** keyed; the
  collection (not a metadata filter) is the scope, which sidesteps ADK's filter-less `SearchRequest`.
- **Recall = ADK-native + ambient.** ADK ships `preloadmemorytool` (auto-injects `SearchMemory`
  results into the system prompt every request — ambient, no model call) + `loadmemorytool`
  (model-callable). Both route through the runner's `MemoryService`, so recall needs **zero custom
  tools** and spreads by attaching the native tool. Scope = which `MemoryService` instance a runner
  holds: node runners (set in `agent.Serve`) get the task store; the orchestrator runner gets the user
  store. The plan's "wire into the executor runner" was corrected — behind A2A the agent executes in
  `agent.Serve`'s runner, which is where the tools' `ctx.SearchMemory` resolves.
- **Embedder — no new model.** Reuses the always-warm CPU `qwen3-embed` group via a new `Embed` path
  on the `openaimodel` adapter; the collection's vector size is **probed** from the embedder on first
  use (no hardcoded dimension).
- **Commit = gated, consolidating (mem0-style).** Agents stage tradecraft via a **`stage_memory`**
  sink tool (its calls land in the worker session); the **trust gate harvests** staged candidates and,
  **only on a judge pass**, fires `memory.Commit` in the **background** (the answer never waits). One
  consolidation pass (the warm **gemma**) **vets** each candidate (drops volatile/junk), **extracts**
  more tradecraft from the accepted answer, and **reconciles** against neighbours →
  **ADD / UPDATE / DELETE / NOOP** with provenance — so a superseding fact updates rather than
  duplicates. The orchestrator writes **user** facts directly via a **`commit_memory`** tool (same
  consolidating `Commit`, scoped to the user collection); user-stated facts are grounded, so ungated.
- **Per-agent guidance** lives in an optional **`memory.md`** bundle file (the second optional file
  alongside `rubric.md`), appended to the agent's prompt **only when memory is on and the agent has its
  memory tools** — so it never dangles. web-researcher's is research tradecraft; the orchestrator's is
  the user profile.
- **Surfacing.** Model-driven memory ops (`stage_memory`, `commit_memory`, `load_memory`) stream as
  ordinary `agent_tool_call` / `agent_tool_result` activity, so they already render in the chat. The
  two **silent platform paths** — ambient `preload_memory` (a request processor, not a tool call) and
  the gate's **background** task-commit — emit no dedicated event; surfacing those would need custom
  events plumbed through the A2A → Translator boundary, deferred as **future polish**.
- No auth, no deploy.

**Done when.** A request recalls prior tradecraft (ambient via `preload_memory`) and it shapes the
research; the agent stages tradecraft, the judge pass commits it (vetted + consolidated), and a later
request recalls it. With `user_memory` on, the orchestrator remembers a stated preference and recalls
it next turn. Judge-failed output is provably **not** written.

**Out of scope (later).** **Consolidation across the whole store** (M6 consolidates per-commit against
neighbours; RecMem-style recurrence-triggered consolidation + Reflexion-style failure memory stay in
Future work); the **doc-corpus** index (the paused Document milestone's OpenSearch FTS / the future `rag-researcher` — a separate
store); auth, deployment.

</details>

## M7 — Skills: hand-authored DAG templates

**Goal.** Give the orchestrator a **catalog of skills** — predetermined DAG recipes it runs
**as authored** — falling back to dynamic M3 planning when none match. Skills **compose existing
agents**; rubrics live on the **agents**, not the plan.

**Scope.**

- **Skill definition**: a skill bundle (`skills/<name>/`) = a `skill.md` (name + **when-to-use**
  description + typed **args / signature**) plus a **plan** definition (the DAG: nodes → agent refs,
  edges → data flow). No rubrics in the skill — each node names an **agent that owns its rubric**.
- **Orchestrator selection**: the planning step first **matches the request against the skill
  catalog** (when-to-use). Match → emit the skill's DAG **verbatim** (immutable, deterministic). No
  match → today's **dynamic decomposition** (M3).
- **Explicit invocation**: an API caller can **name a skill + args** directly, bypassing auto-select
  (the deterministic entry that uploads / webhooks use).
- **Executor**: a skill-built DAG runs through the **existing** topological + vetted executor — no new
  execution path.
- **Rubric relocation**: rubrics move onto **agents** (`agents/<name>/rubric.md`); the M3 idea of the
  planner writing a per-node rubric is **dropped for skill-built DAGs** (the dynamic fallback may still
  synthesize one).
- **Agents are a reusable library; names are agentive roles**: agents are named by **role / capability
  in agentive `<domain>-<actor>` form** (`web-researcher`, `code-reviewer`, `media-reader`,
  `image-reader`, `classifier`, `document-organizer`) — **never** by a bare DAG-step operation
  (`ocr`, `summarize`). A **`general-purpose`** agent (generalist text worker) handles steps that need
  no specialist. **Skill steps select an agent from this library** — there is **no 1:1 step→agent
  mapping**: one agent serves many steps, and a step picks a specialist only when it earns one. (This
  is exactly A2A's model — `AgentCard` = one role identity, operations live in `AgentSkill` / the skill
  DAG; ADK requires the name be a unique identifier and reserves `user`.)
- **APIs**: skills are listable; a task records **which skill** (if any) produced its DAG.

**Done when.** A hand-authored skill is **auto-selected** by the orchestrator and executed as authored;
the **same skill** can be invoked **explicitly** by name + args; an unmatched request still **falls
back** to dynamic planning. Every node is vetted against its **agent's own** rubric.

**Out of scope (later).** The `doc-ingest` and `code-review` skills themselves (the paused Document / Code-review milestones); mid-DAG
human parking (removed from M5; would need reviving).

## M8 — Usability / interactive control & operability

**Goal.** Make runs **controllable and durable**: persist tool calls + node lifecycle, control
runs/nodes (stop / start / cancel / **steer**), **queue + interrupt** requests, spread work via **HRW
(rendezvous) routing**, and drive it all from a **Go CLI**. This is the operability layer — inspired
by what Turnstone ships that Quack lacked.

> **Status (2026-07): mostly shipped.** Durable event log + reconnect (PR #115), per-node
> cancel/retry (PR #120), mid-node steering (PRs #108–#112), and the Go CLI (`cmd/quack`, a full
> Bubble Tea TUI) are merged. Remaining: **request queuing + interrupt** and **HRW routing** —
> both specced pre-ADK v2, so re-scope before building.

**Scope.**

- ✅ **Durable event log** (PR #115): persist the run event stream (`dag_plan`, node lifecycle,
  `agent_tool_call` / `agent_tool_result`, tokens) to Postgres, backing the SSE hub's replay with
  `Last-Event-ID` resume. Buys reconnect / multi-device / post-restart replay **and** a tool-call
  **audit trail**. Known follow-up: client-side replay of a run killed by a restart.
- ✅ **Run + node lifecycle** (PR #120 + M5b): whole-run cancel extended to **per-node** cancel and
  **retry of a finished node**, with cancelled status surfaced in both UIs.
- ✅ **Node steering (HITL)** (PRs #108–#112, shipped early as M5b): inject a steer message into a
  **running** node to redirect it mid-run. Shipped **without** the pause/resume revival this bullet
  originally planned — steering interrupts the node's context and **re-runs the same session** with
  the guidance appended (prior tool results retained). Node-level *pause for human input* (which
  M9's `bash` approval needs) is the separate native-HITL effort on `feat/node-hitl`.
- **Request queuing + interrupt** *(open)*: a bounded **inter-request** run queue at the chat entry
  (at capacity → queue, not reject) with **interrupt** of a queued or in-flight run. Distinct from
  `dag.max_active_nodes` (intra-DAG node concurrency).
- **HRW routing** *(open — re-scope post-ADK v2)*: a Highest-Random-Weight router (FNV-1a of
  `node_id` × backend id) to spread node/agent dispatch across worker backends with minimal
  reshuffle on join/leave — pulls the Future-work *Distributed A2A* seam's routing primitive
  forward. Revisit whether it still earns its place under the ADK v2 engine.
- ✅ **Go CLI** — shipped beyond spec: `cmd/quack` is a full **Bubble Tea TUI** (chat, DAG view,
  node inspector, steering, clarification prompts) plus the `server init` wizard and `-p` print
  mode, over the REST API via the generated client.

**Done when.** A run **replays fully after an app restart**; a node is **cancelled / restarted /
steered** mid-run from the UI or CLI; requests past capacity **queue** and an in-flight one can be
**interrupted**; node dispatch **spreads via HRW** across ≥2 backends; the **CLI** drives a chat end
to end.

**Out of scope (later).** Standalone distributed A2A services (M8 ships the HRW routing primitive
only); multi-tenant scheduling / fairness; auth (M11).

## M9 — Primitive file-system + shell tools

**Goal.** Give agents a **portable primitive toolset** — sandboxed file read / write / edit / list +
ripgrep search, plus a **judge-gated `bash`** — so "documents" are **files on disk** and search is
**grep**, replacing the bespoke record / FTS / vector stack (the paused Document milestone). Direction
validated by Turnstone (files + ripgrep + bash, no document DB).

**Scope.**

- **File tools** (`read_file` / `write_file` / `edit_file` / `list_dir` / `grep`) confined to a
  configured **workspace root** (`workspace.root`), paths guarded against `..` traversal. `grep` is a
  typed wrapper over **ripgrep**. Writes are bounded by the sandbox, so they need no gate.
- **`bash` + 3-tier safety gate** (auto-allow polarity, Turnstone-shaped): (1) a deterministic
  **allowlist** of read-only commands → auto-run + a **denylist** of foot-guns → force-ask; (2) a
  cheap **safety judge** (reusing the warm gemma judge) for the gray zone → `allow | ask | deny`; (3)
  on `ask`, **HITL approval** — via `get_user_choice` at the orchestrator and M8's node steering
  inside a node — with the approval persisted (a matching command runs; mirrors Turnstone's
  `intent_verdicts`).
- **Documents become files**: "save this note" writes a markdown file (title / series / date as
  front-matter or path convention); "find the note about X" greps the workspace. The
  document-subsystem record store / FTS / doc-vector index is reverted; **Qdrant stays for M6 memory
  only**.

**Done when.** A chat **writes a note to a file** and **`grep` finds it**; a read-only `bash` (`ls`)
**auto-runs**; a risky `bash` (`rm …`) **escalates to approve/deny** and only runs on approval; a
`../` path is **rejected**.

**Out of scope (later).** Container / namespace isolation of `bash` (it's cwd-confined + judge-governed
for now); any document DB / FTS / semantic doc search (the paused Document milestone).

## M10 — CI/CD release automation

**Goal.** Cut **versioned releases** automatically: publish the `quack` **CLI binaries** to GitHub
Releases and the server **Docker image** to GHCR on a release tag — so the CLI (M8) and the server are
distributable artifacts, not just a local build.

**Scope.**

- **Release workflow** (`.github/workflows/release.yml`), triggered on a **semver tag** (cut after a
  merge to `main`). Builds the frontend first (the SPA is embedded), then runs **goreleaser**.
- **CLI binaries**: cross-compile `cmd/cli` (`linux` / `darwin` × `amd64` / `arm64`) → a **GitHub
  Release** with checksums.
- **Docker image**: build the server image (existing `Dockerfile`) → push to **GHCR**
  (`ghcr.io/fagerbergj/quack`), tagged version + `latest`.
- **Version stamping**: inject the version via `-ldflags` so `quack version` and the server report it.
- **Merge-driven versioning (optional)**: a `release-please` workflow opens a release PR on merge to
  `main`; merging it tags the release, firing the workflow. Separate from the existing CI (the
  *publish* path, not the *test* path).

**Done when.** Tagging a release (or merging a release-please PR) yields a `docker pull`-able **GHCR
image** and a **GitHub Release** with downloadable **CLI binaries**, both stamped with the version.

**Out of scope (later).** Production deploy behind the gateway (M11); release signing / SBOM /
provenance attestation; Homebrew tap / apt distribution.

## M11 — Auth + deploy

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
- **Public webhook ingress**: the deploy exposes the **public endpoints** the GitHub App (the paused
  Code-review milestone) and any reMarkable webhook (the paused Document milestone) would post to.
  General **outbound / delegated (act-as-user) tool auth** beyond an installation token is still out of
  scope.

**Done when.** Quack runs deployed behind the gateway; an authenticated SPA user (via the IdP login)
and a token-bearing MCP/A2A client can both drive it end to end; unauthenticated requests are
rejected; the spec is live in the gateway docs.

**Out of scope (later).** General outbound / delegated (act-as-user) tool auth beyond a GitHub App
installation token (the paused Code-review milestone); broader researcher build-out (more agents /
tools, RAG / `rag-researcher`).

## M12 — Adaptive / content-dependent re-planning

**Goal.** Let the orchestrator **re-plan the DAG as results arrive**, so it can act on content it could
not know up front — transcribe an audio clip, *then* plan the work the transcript asks for, in a
**single turn** (versus M4's two-turn workaround). This is the capability M4 deliberately defers.

**Scope.**

- **Adaptive executor**: after a node reveals content (a transcript, an OCR'd document, a search
  result), the orchestrator may **extend / revise the remaining DAG** instead of running a fixed plan.
- **Re-plan triggers + budget caps**: bounded by **max re-plans / depth** so it can't loop.
- **Supersedes M4's two-turn handling** for media, and lets a file-ingest flow re-route on what a
  document actually contains.
- Builds on **M3**'s planner and **M7** skills (a skill may declare explicit re-plan points).

**Done when.** A single turn — an audio clip with "do what this asks" — transcribes it and then the
orchestrator **plans + runs** the requested work, with no second turn needed.

**Out of scope (later).** Anything not about re-planning.

## M13 — Observability (OTel → Prometheus + Grafana)

**Goal.** Make a running Quack **observable**: emit OpenTelemetry **traces + metrics** following the
GenAI semantic conventions and ship them to the **existing home-server monitoring stack** (Prometheus +
Grafana on `jason-server`), so agent latency, tool calls, token usage, and errors are visible per
node / per agent. The DAG already streams to the UI for *live* watching; this is the **durable,
queryable** view across runs.

**Scope.**

- **OTel SDK wiring**: configure trace + metric providers in `cmd/server/main.go`, exporting **OTLP/gRPC**
  to an OTel Collector endpoint (`OTEL_EXPORTER_OTLP_ENDPOINT`). Prefer ADK Go's `telemetry` package
  (`telemetry.New` + custom `SpanProcessors`, `WithOtelToCloud(false)`) so ADK's built-in agent/tool
  spans flow through; fall back to standard `OTEL_EXPORTER_OTLP_*` env vars if the package's seam is
  awkward. `service.name = quack`.
- **What we emit**: ADK's native GenAI spans (`invoke_agent`, `execute_tool`, `generate_content {model}`
  with `gen_ai.usage.*_tokens`, `error.type`) and metrics (`gen_ai.agent.invocation.duration`,
  `gen_ai.tool.execution.duration`, request/response size, workflow steps). **Reuse ADK's
  instrumentation**; only add Quack-specific spans where the pipeline isn't ADK-native (planner DAG
  decompose, the trust gate / judge stage) — one span each, not a re-instrumentation of everything.
- **Collector (home-server)**: add an **`otel-collector`** service to `home-server/monitoring/` — OTLP
  receiver on `:4317`/`:4318`, `batch` + `memory_limiter` processors, **`prometheus` exporter** on
  `:8889` (namespace `quack`). Add a scrape job for `otel-collector:8889` to
  `monitoring/prometheus/config.yaml`. Traces export to `debug` for now (a Tempo sink is future work).
- **Grafana dashboard**: drop a `quack-observability.json` into `monitoring/grafana-dashboards/`
  (invocation p50/p90/p99, tool latency by tool, error rate, tokens) — provisioned the same way as the
  existing dashboards.
- **Config + ports**: OTel endpoint is config-driven and **off by default** (no collector → no-op
  exporter, no startup failure). Document the env var in `configuration.md` and the new container in the
  monitoring README.

**Done when.** With the collector running, driving a Quack request produces traces (agent → tool →
model spans) and metrics scraped into Prometheus, and the Grafana dashboard shows per-agent invocation
latency, tool-call latency, token usage, and error rate. With no collector configured, Quack starts and
runs unchanged.

**Out of scope (later).** A **Tempo** (or Jaeger) trace sink + trace UI in Grafana (collector exports
traces to `debug` only for now); Grafana's first-party **AI Observability** plugin / generation
analytics; **alerting / SLOs** on the new metrics; log aggregation (Loki).

---

## Paused (deferred)

Milestones taken **out of the active sequence**. Their bodies are kept as the historical rationale
(like M5b's "Removed" note); pick them back up when priorities shift.

### ⏸ Document ingestion (paused)

> **Paused.** The early build (Postgres records + OpenSearch FTS + Qdrant doc-vectors + doc agents)
> was **reverted** — too bespoke and not portable. The "save a note, find it later" goal is now served
> by **M9**'s primitive file tools (documents = files on disk, search = grep). The original plan is
> preserved below for if a heavier document pipeline is ever revived.

**Goal.** Replace **document-pipeline** with a **`doc-ingest` skill** — a hand-authored DAG that
**ports doc-pipeline's stages as quack agents / tools**, ingesting images / audio / text into an
**OpenSearch full-text index** you can query. Builds on M4 (vision / audio), M7 (skills), and M5
(upfront clarify).

**Scope.**

- **Steps select agents from the library (not 1:1)**: doc-pipeline's stages map onto **skill steps**,
  each picking the right agent — printed-text OCR → **`media-reader`**; handwriting → the
  **`image-reader`** specialist; transcribe → **`media-reader`**; clarify & summarize → a
  **`general-purpose`** agent (or a specialist if a
  step earns one); classify → a **`classifier`** agent; and a final **`document-organizer`** agent
  gathers the artifacts (raw text, summary, tags) and writes them to persistent storage. doc-pipeline's
  mechanical **tool logic** (image encoding, chunking, OpenSearch indexing) ports **as-is as builtin
  tools** those agents call.
- **`doc-ingest` skill**: a hand-authored DAG — *ingest → (OCR | transcribe) → summarize → clarify →
  classify → index* — auto-selected or explicitly invoked (M7). Mid-DAG node parking for an unsure
  clarify / classify step was removed from M5; reviving it would let those steps ask the human.
- **Retrieval = OpenSearch FTS only**: index documents (title, tags, summary, content, series, date)
  into **OpenSearch** for keyword / Lucene search. **No** Qdrant / embeddings / contextual chunking for
  docs. **`series` kept as a metadata grouping / filter** (no concatenate-and-embed).
- **Ingestion entry**: **explicit skill invocation** — a **file-upload** endpoint names `doc-ingest`
  with the file as its arg; the **reMarkable webhook** delivers tablet pushes as explicit invocations
  (validated once deployed, M11).
- **Storage**: the **blob store** (M4) holds source media + artifacts; the relational store holds
  document / job / stage records.
- **Frontend**: a **documents / jobs** view to browse ingested docs + stage outputs and **answer
  parked** clarify / classify prompts.

**Done when.** Upload a photo of a handwritten note (or an audio clip, or text) → `doc-ingest` OCRs /
transcribes it, summarizes, **parks for clarification** if low-confidence, classifies / tags, and
**indexes it into OpenSearch**; you find it by keyword / tag / series query. **document-pipeline can be
retired.**

**Out of scope (later).** Semantic / vector RAG over docs (intentionally dropped — revisit only if FTS
proves insufficient); porting every doc-pipeline dashboard feature (iterative).

### ⏸ Code review — GitHub App (paused)

> **Paused.** GitHub-review work is on hold; revisit after the active path. It still needs M11's deploy
> + public endpoint.

**Goal.** A **`code-review` skill** that reviews a PR via a **GitHub App** and posts **inline
comments**. **One** reusable `code-reviewer` agent; the **skill encodes the strategy** (which instances
to spawn and how to prompt them), so fan-out strategies are swappable without touching the agent. Needs
M11's deploy + public endpoint.

**Scope.**

- **GitHub App**: a registered App receives webhooks; a **`/quack review` PR comment** (explicit
  invocation) triggers the review. **Installation-token** auth (the outbound-auth seam) fetches the
  diff and posts review comments.
- **`code-reviewer` agent**: **one** agent (model + tools + its **own rubric**) that reviews a slice of
  a diff, reused across nodes.
- **`code-review` skill**: a hand-authored DAG that spawns **multiple `code-reviewer` instances**, each
  prompted per a **strategy** (by-dimension: correctness / security / simplification — or
  by-functionality) → a **synthesizer** node joins the findings. Strategy lives in the **skill**, so
  it's swappable without touching the agent. Each node is vetted against the agent's rubric.
- **Output**: **inline PR review comments** on specific lines, with **anchoring + dedup** on re-runs.
- **Tools**: a **GitHub client** (fetch PR diff / files, post review comments) as builtin tools.
- **Dogfooding**: install on your repos **including quack itself** — `/quack review` on a quack PR
  posts inline findings.

**Done when.** Commenting **`/quack review`** on a PR makes quack (via the App) fetch the diff, run the
`code-review` skill (fanned-out `code-reviewer` instances → synthesizer, each vetted), and **post
inline review comments**. Swapping the skill's **strategy** changes the fan-out without changing the
agent.

**Out of scope (later).** Auto-review on every PR open (explicit `/quack review` trigger only);
check-run / merge-gating output; non-GitHub forges.

---

## Future work (beyond the active milestones)

Everything below is intentionally outside the M0–M13 plan, captured so it is not lost. Most are
"extensible in theory" seams we shaped but did not build.

| Theme | Item | Notes |
| --- | --- | --- |
| Auth | **Outbound tool auth** | Per-tool `auth.kind`: `api_key`, `client_credentials` (OAuth2 M2M), `delegated` (act-as-user). Only inbound OIDC is built. `delegated` needs a per-user token store + consent flow. |
| Inference | **More model providers** | `gemini`, `anthropic` provider `kind`s; only `openai` is implemented. |
| Stores | **More store backends** | `sqlite` (relational), `pgvector` (vector); only Postgres + qdrant are implemented. |
| Tools | **More tool kinds** | `mcp` (consume external MCP servers' tools via ADK `mcptoolset`) and `http` (declarative HTTP tools); only `builtin` is implemented. |
| Vetting | **70B Selene escalation** | Escalate high-stakes / low-confidence nodes to the batched 70B `selene` judge. The CPU `gemma4-26b-a4b` is the single gate for now. |
| Vetting | **Deterministic floor** | A `platform/verify` pass (citation grounding, source provenance, quote fidelity, schema, URL liveness, code/tests) that runs before the judge. Pulled because it forces structured agent output, a bigger decision. |
| Vetting | **Tool-grounded critique** | CRITIC-style critics that call tools to verify claims rather than reason alone. |
| Vetting | **Per-agent adversarial overrides** | Global adversarial policy only for now; later, per-agent judge / threshold / rounds overrides. |
| Agents | **Distributed A2A** | Promote agents from co-located to standalone A2A services (the design is already A2A-ready). |
| Agents | **Orchestrator A2A face** | Expose the orchestrator as an A2A server to external agent clients (M1's A2A is internal orchestrator to agent dispatch). |
| Memory | **Store-wide consolidation** | M6 consolidates **per-commit** against nearest neighbours (mem0-style ADD/UPDATE/DELETE/NOOP); later, RecMem-style recurrence-triggered consolidation + dedup / merge **across the whole store**. |
| Memory | **Reflexion-style memory** | Store language reflections on failures, not just vetted findings. |
| Memory | **Metadata-filtered recall** | ADK's `SearchRequest` is query-only; later, filter recall by source / date / score (needs a custom search path beyond the ADK interface). |
| Memory | **Dedicated surfacing** | Model-driven memory tools render as tool activity, but ambient `preload_memory` recall + the gate's background commit are invisible (platform paths, not tool calls). Add `memory_recall` / `memory_commit` events through the A2A → Translator boundary + chat rendering if the silent paths need showing. |
| Research | **Researcher build-out** | `rag-researcher` + RAG, more agents/tools, and the second example use case ("latest local LLM models for my hardware"). |
| Documents | **Semantic RAG over docs** | only relevant if the paused Document milestone is revived; revisit Qdrant embeddings + an ask-and-cite chat over the corpus if M9's grep-over-files proves insufficient. |
| Documents | **Contextual chunking** | The small-LLM per-chunk context blurb from doc-pipeline — only relevant if semantic RAG returns. |
| Code review | **Auto-review on PR open** | the paused Code-review milestone is explicit `/quack review` only; later, auto-trigger on PR open / sync via the App webhook. |
| Code review | **Check-run / merge gating** | the paused Code-review milestone posts inline comments; later, a GitHub Check Run with pass / fail that can gate merge. |
| Multi-modal | **Audio / image generation** | Input-only for now (vision + STT); later, TTS / image output. |
| Skills | **Self-authored skills** | Agent self-improvement: the orchestrator promotes a **successful dynamic DAG (M3)** into a **saved skill (M7)** — crystallizing proven plans into the reusable catalog so the system grows its own repertoire instead of re-planning from scratch. Needs a quality bar (only promote vetted / repeated wins) + a review/approval step before a skill goes live. |
| Email | **Email agents + tools** | An inbox assistant (agentive role, e.g. `inbox-manager`) with builtin tools to **read, summarize, reply, and clean** a mailbox. Reads are straightforward; **sends / deletes are side-effecting**, so they route through a `RequireConfirmation` gate (the paused Code-review milestone), and the whole integration needs **delegated (act-as-user) outbound auth** (the Auth row). |
| Frontend / UX | **Virtual scrolling** | The chat renders all turns with a plain `.map()`. The 2025-06 UX pass isolated re-renders (extracted `Composer`/`ChatList`, memoized `TurnView`) so completed turns no longer recompute mid-stream — which closes the typing-lag complaint without windowing. Add `react-virtuoso` only if very long chats still lag at the DOM-node count. |
| Frontend / UX | **Structured citations** | Sources are currently model-authored `<details><summary>Sources</summary>` blocks inside the answer markdown. A real citation model (cites as data on the turn, inline superscript links → source cards) needs an output-contract change in the agents + `openapi.yaml`, so it's not UI-only. |
| Frontend / UX | **Edit / retry messages** | Edit & resubmit a past user message; retry a response. Needs new turn-state handling (truncate-and-resubmit) beyond the current append-only chat. |
