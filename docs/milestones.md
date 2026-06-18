# Quack — Milestones

The build plan. Each milestone is an **end-to-end increment** with a clear "done when", not a
layer in isolation. Architecture lives in the project `README.md`;
per-choice config in [configuration.md](configuration.md).

Format per milestone: **Goal · Scope · Done when · Out of scope**.

---

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

---

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

---

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

---

## M3 — DAG planning + execution (with visualization) ✅

<details>
<summary><strong>✅ Complete.</strong> The orchestrator decomposes a request into a <strong>DAG</strong>
of agent nodes and runs it topologically — each node wrapped in M2's trust gate — with a tool-less
<code>synthesizer</code> joining the results. The DAG streams over the APIs (node-lifecycle events) and
animates live in the UI (<code>DagView</code> / <code>DagNode</code>). <em>Inferred complete from the
code; confirm the Dublin trip-planning demo is validated.</em></summary>

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

---

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
follow-up turn acts on it); a content-dependent DAG is **M11**.

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
  **calls `request_input` (M5)** to ask the human — agent-driven, not an automatic confidence gate; a
  consensus-ASR fidelity tool is optional future hardening.
- **Capability tags**: providers / agents gain **`text` / `vision` / `audio`** tags so the factory
  routes parts to a capable model and rejects a mismatch at dispatch.
- **Surfacing**: attachments render in the chat UI; the agent's answer streams as usual.

**Done when.** Attach a photo and ask about it → **`media-reader`** answers; attach a photo of a
**handwritten note** and ask to transcribe → the orchestrator routes to the **`image-reader`**
specialist (`qwen3-vl-32b`); attach an audio clip → **`media-reader`** transcribes it. Native
`image_url` / `input_audio` throughout; the result **persists as text in history**, so a follow-up
turn can act on it. The orchestrator does **not** plan a content-dependent DAG on unknown media
(that's M11).

**Out of scope (later).** Content-dependent / adaptive DAG planning on media (**M11**); **video** (a
later group + agent); the document-ingestion pipeline (**M8**); embedding / indexing.

</details>

---

## M5 — Human-in-the-loop (clarify + suspend/resume)

**Goal.** Two HITL capabilities: **(a)** the orchestrator **clarifies an ambiguous ask before
launching a DAG**, and **(b)** a running node **pauses for human input and the DAG resumes** once
answered — built on Go ADK's **native long-running-tool seam** surfacing as A2A's non-terminal
**`input-required`** task state, so a paused run **survives restarts**. The pause is always
**agent-driven** (an agent decides it needs a human); there is **no automatic confidence gate** — the
M2 judge keeps its continue-but-warn behavior.

**Scope.**

- **Upfront clarification — no suspend machinery.** Before planning, the orchestrator may return a
  **clarifying question instead of a DAG** — an ordinary conversational turn. The user's reply is the
  next message; the planner re-plans with the clarification in history (`buildHistory` already carries
  it), asking again if still ambiguous. No parked state, no resume API — the existing chat loop covers
  this case entirely.
- **Mid-DAG pause = ADK long-running tool → A2A `input-required`.** Reuse Go ADK's built-in
  pause/resume seam rather than reinventing it: an agent calls a **long-running function tool**, whose
  call ID lands in `event.LongRunningToolIDs`, ending the agent turn cleanly with the call unresolved;
  `server/adka2a` converts that into the non-terminal **`TaskStateInputRequired`** status carrying the
  pending `FunctionCall`. `remoteagent` keeps the task alive (it does not cancel `input-required`).
  Two tool shapes, both **opt-in per agent** (declared in the agent's tool selection — only agents
  meant to pause get them):
  - **`request_input`** — an **authored** free-form tool (Go ADK lacks Python's `get_user_choice`):
    the agent poses an open question ("which Dublin?", "is this word 'meet' or 'meat'?").
  - **`RequireConfirmation`** — ADK's **native typed** approve/deny + payload gate, for
    **side-effecting tools** (the seam M10's GitHub writes will use). Same long-running plumbing — built
    once here, so the Future-work "confirmation gate" item folds into M5.
- **Agents signal, the orchestrator asks.** Agents never address the user directly; they only emit the
  pending call. The **orchestrator** (the A2A client, the sole user-facing component) surfaces the
  question and collects the answer — A2A-native, so it works identically once agents become standalone
  services (the Distributed-A2A future item).
- **Resume = stateful continuation, not re-run.** Each node already runs in a **persisted** ADK
  session (`plan.ID + ":" + node.ID`, Postgres-backed). Resume appends the human answer as the matching
  **`FunctionResponse`** to that same session and re-runs on the same `TaskID` / `ContextID`; ADK
  replays from persisted events, so the agent keeps the reasoning it did **before** asking. Survives
  restarts.
- **Readiness-driven executor — replaces the layer barrier.** M3's strict per-layer barrier becomes a
  scheduler that runs a node **as soon as its `DependsOn` are all `done`**. A node that calls a
  long-running tool enters a new **`waiting`** state (detected by a non-empty `LongRunningToolIDs` on
  the run's final event); its descendants block, but **independent branches keep running**. The whole
  DAG **suspends and the request ends only when the runnable frontier is empty *and* ≥1 node is
  `waiting`**.
- **Persistence + resume API.** Extend the existing `dag_plans` / `dag_nodes` tables: persist **full
  node outputs** (today only a 250-char `output_preview` is stored) so a fresh process can feed
  downstream nodes, plus each parked node's **`waiting` status, `TaskID` / `ContextID`, and question /
  confirmation prompt**. (The node's own conversation is already persisted by ADK's SessionService — not
  duplicated.) A **resume endpoint** `(planID, nodeID, answer)` appends the `FunctionResponse`,
  rehydrates the scheduler from persisted outputs, and **reopens an SSE stream** to run to completion
  (or the next pause).
- **API + events.** A new **`node_waiting`** lifecycle event carries the question / approve-deny prompt;
  the UI surfaces parked nodes with an answer / approve control. A paused turn leaves the orchestrator
  turn **incomplete** until resume writes the final answer (`buildHistory` tolerates the half-finished
  turn).

**Done when.** An ambiguous request triggers an **upfront** clarifying question before any DAG runs.
Separately, a node calls `request_input` mid-DAG → it enters `input-required`, **independent branches
keep running**, the run **persists and the request ends**; submitting an answer **resumes the same
node statefully** and the DAG completes. A side-effecting tool guarded by `RequireConfirmation` pauses
for **approve / deny** the same way. All visible in the stream and **across a restart**.

**Out of scope (later).** The `doc-ingest` / `code-review` skills that consume parking (M8 / M10);
automatic confidence-based parking (parking is always agent-driven).

---

## M6 — Memory (recall + vetted commit)

**Goal.** Give agents **durable, semantic memory**: recall prior **vetted findings** to inform new
work and commit new ones — gated by M2's trust gate so **only judge-passed output is ever written**.
Go ADK ships only a 2-method `memory.Service` interface + `load_memory` / `preload_memory` tools and
**no self-hostable backend** (just in-memory keyword + GCP Vertex), so M6 **authors a Qdrant-backed
`memory.Service` + an embedder**, with writes driven explicitly from the post-judge hook (ADK never
auto-writes).

**Scope.**

- **Custom `memory.Service` over Qdrant.** Implement ADK's `memory.Service` (`AddSessionToMemory` +
  `SearchMemory`) backed by a **Qdrant** collection (new `docker-compose` service — the vector store
  the architecture already names in M9 / the Stores row). Set it on the same `runner.Config`'s
  `MemoryService` the executor's runners already build, so recall is wired wherever a node or the
  planner runs. Memories are **keyed per-user** (ADK's `SearchRequest.UserID`).
- **Embedder — already running, no new model.** llama-swap already serves **`qwen3-embed`**
  (`Qwen3-Embedding-8B` Q8_0, `--embedding --pooling last`, OpenAI `/v1/embeddings`) in a
  **`persistent`, CPU-only** group (`-ngl 0`, GPUs hidden) that **stays warm regardless of GPU swaps**
  at **zero VRAM cost** — exactly the no-swap-churn property recall / commit need. So M6 stands up
  **no embedding model**: a new `embedding` config block points the `openaimodel` adapter's new
  **`Embed` path** at the existing endpoint.
- **Commit = distilled facts, gated by the judge.** When the **terminal (answer) node passes vetting**
  (`internal/vetting/gate.go`, on `passed` only), a platform **distillation step** — reusing the
  already-warm **judge model (gemma)** — condenses the vetted answer into **atomic fact(s)**, each
  stored with **provenance** (source turn, citations, judge score, timestamp) so a recalled fact stays
  citable. The trust gate is the literal gatekeeper; unvetted output is never written. This post-judge
  hook is the **only** writer (ADK's runner does not auto-call `AddSessionToMemory`).
- **Recall = preload + tool (both).** The **planner preloads** relevant facts (an automatic
  `SearchMemory` on the request) so the **DAG is memory-aware** from the start; **research agents
  additionally carry the `load_memory` tool** for deliberate, deeper recall mid-task. Both reach the
  same per-user Qdrant store via the runner's `MemoryService`.
- **Surfacing.** Recall (which facts were pulled) and commit (which facts were distilled + written)
  **stream to the UI and over the APIs** as activity, like thinking / tool events.
- No auth, no deploy.

**Done when.** A request **recalls** prior vetted facts from Qdrant (visible in the stream) and they
shape the answer; the new vetted answer is **distilled into atomic facts and committed**; a later
request **recalls one of those facts** and cites it. Judge-failed output is provably **not** written.

**Out of scope (later).** Memory **consolidation / dedup** (append-only for now — RecMem-style
recurrence consolidation + Reflexion-style failure memory stay in Future work); **metadata-filtered**
search (ADK's `SearchRequest` is query-only — no filter param); the **doc-corpus** index (that's M8's
OpenSearch FTS / the future `rag-researcher` — a separate store from this findings memory); auth,
deployment.

---

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

**Out of scope (later).** The `doc-ingest` and `code-review` skills themselves (M8 / M10); mid-DAG
human parking (M5).

---

## M8 — Document ingestion (retire document-pipeline)

**Goal.** Replace **document-pipeline** with a **`doc-ingest` skill** — a hand-authored DAG that
**ports doc-pipeline's stages as quack agents / tools**, ingesting images / audio / text into an
**OpenSearch full-text index** you can query. Builds on M4 (vision / audio), M7 (skills), and M5
(clarify / classify parking).

**Scope.**

- **Steps select agents from the library (not 1:1)**: doc-pipeline's stages map onto **skill steps**,
  each picking the right agent — printed-text OCR → **`media-reader`**; handwriting → the
  **`image-reader`** specialist; transcribe → **`media-reader`**; clarify & summarize → the
  **`general-purpose`** agent (or a specialist if a
  step earns one); classify → a **`classifier`** agent; and a final **`document-organizer`** agent
  gathers the artifacts (raw text, summary, tags) and writes them to persistent storage. doc-pipeline's
  mechanical **tool logic** (image encoding, chunking, OpenSearch indexing) ports **as-is as builtin
  tools** those agents call.
- **`doc-ingest` skill**: a hand-authored DAG — *ingest → (OCR | transcribe) → summarize → clarify →
  classify → index* — auto-selected or explicitly invoked (M7), where the **clarify / classify agents
  call `request_input` (M5) when unsure** — agent-driven parking, not an automatic gate.
- **Retrieval = OpenSearch FTS only**: index documents (title, tags, summary, content, series, date)
  into **OpenSearch** for keyword / Lucene search. **No** Qdrant / embeddings / contextual chunking for
  docs. **`series` kept as a metadata grouping / filter** (no concatenate-and-embed).
- **Ingestion entry**: **explicit skill invocation** — a **file-upload** endpoint names `doc-ingest`
  with the file as its arg; the **reMarkable webhook** (kept) delivers tablet pushes as explicit
  invocations (validated once deployed, M9).
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

---

## M9 — Auth + deploy

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
- **Public webhook ingress**: the deploy exposes the **public endpoints** the GitHub App (M10) and the
  reMarkable webhook (M8) post to. General **outbound / delegated (act-as-user) tool auth** beyond the
  GitHub App's installation token (M10) is still out of scope.

**Done when.** Quack runs deployed behind the gateway; an authenticated SPA user (via the IdP login)
and a token-bearing MCP/A2A client can both drive it end to end; unauthenticated requests are
rejected; the spec is live in the gateway docs.

**Out of scope (later).** General outbound / delegated (act-as-user) tool auth beyond the GitHub App
(M10); broader researcher build-out (more agents / tools, RAG / `rag-researcher`).

---

## M10 — Code review (GitHub App)

**Goal.** A **`code-review` skill** that reviews a PR via a **GitHub App** and posts **inline
comments**. **One** reusable `code-reviewer` agent; the **skill encodes the strategy** (which instances
to spawn and how to prompt them), so fan-out strategies are swappable without touching the agent. Needs
M9's deploy + public endpoint.

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

## M11 — Adaptive / content-dependent re-planning

**Goal.** Let the orchestrator **re-plan the DAG as results arrive**, so it can act on content it could
not know up front — transcribe an audio clip, *then* plan the work the transcript asks for, in a
**single turn** (versus M4's two-turn workaround). This is the capability M4 deliberately defers.

**Scope.**

- **Adaptive executor**: after a node reveals content (a transcript, an OCR'd document, a search
  result), the orchestrator may **extend / revise the remaining DAG** instead of running a fixed plan.
- **Re-plan triggers + budget caps**: bounded by **max re-plans / depth** so it can't loop.
- **Supersedes M4's two-turn handling** for media, and lets **doc-ingest (M8)** re-route on what a
  document actually contains.
- Builds on **M3**'s planner and **M7** skills (a skill may declare explicit re-plan points).

**Done when.** A single turn — an audio clip with "do what this asks" — transcribes it and then the
orchestrator **plans + runs** the requested work, with no second turn needed.

**Out of scope (later).** Anything not about re-planning.

---

## M12 — Observability (OTel → Prometheus + Grafana)

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

## Future work (beyond M11)

Everything below is intentionally outside the M0–M11 plan, captured so it is not lost. Most are
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
| Memory | **Consolidation / dedup** | M6 commits facts append-only; later, RecMem-style recurrence-triggered consolidation + dedup / merge of overlapping facts. |
| Memory | **Reflexion-style memory** | Store language reflections on failures, not just vetted findings. |
| Memory | **Metadata-filtered recall** | ADK's `SearchRequest` is query-only; later, filter recall by source / date / score (needs a custom search path beyond the ADK interface). |
| Research | **Researcher build-out** | `rag-researcher` + RAG, more agents/tools, and the second example use case ("latest local LLM models for my hardware"). |
| Documents | **Semantic RAG over docs** | doc-ingest ships FTS-only (M8); revisit Qdrant embeddings + an ask-and-cite chat over the corpus if keyword / FTS proves insufficient. |
| Documents | **Contextual chunking** | The small-LLM per-chunk context blurb from doc-pipeline — only relevant if semantic RAG returns. |
| Code review | **Auto-review on PR open** | M10 is explicit `/quack review` only; later, auto-trigger on PR open / sync via the App webhook. |
| Code review | **Check-run / merge gating** | M10 posts inline comments; later, a GitHub Check Run with pass / fail that can gate merge. |
| Multi-modal | **Audio / image generation** | Input-only for now (vision + STT); later, TTS / image output. |
| Skills | **Self-authored skills** | Agent self-improvement: the orchestrator promotes a **successful dynamic DAG (M3)** into a **saved skill (M7)** — crystallizing proven plans into the reusable catalog so the system grows its own repertoire instead of re-planning from scratch. Needs a quality bar (only promote vetted / repeated wins) + a review/approval step before a skill goes live. |
| Email | **Email agents + tools** | An inbox assistant (agentive role, e.g. `inbox-manager`) with builtin tools to **read, summarize, reply, and clean** a mailbox. Reads are straightforward; **sends / deletes are side-effecting**, so they route through M5's `RequireConfirmation` gate, and the whole integration needs **delegated (act-as-user) outbound auth** (the Auth row). |
| Frontend / UX | **Virtual scrolling** | The chat renders all turns with a plain `.map()`. The 2025-06 UX pass isolated re-renders (extracted `Composer`/`ChatList`, memoized `TurnView`) so completed turns no longer recompute mid-stream — which closes the typing-lag complaint without windowing. Add `react-virtuoso` only if very long chats still lag at the DOM-node count. |
| Frontend / UX | **Structured citations** | Sources are currently model-authored `<details><summary>Sources</summary>` blocks inside the answer markdown. A real citation model (cites as data on the turn, inline superscript links → source cards) needs an output-contract change in the agents + `openapi.yaml`, so it's not UI-only. |
| Frontend / UX | **Edit / retry messages** | Edit & resubmit a past user message; retry a response. Needs new turn-state handling (truncate-and-resubmit) beyond the current append-only chat. |
