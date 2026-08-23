# AGENTS.md

This file provides guidance to AI coding agents working in this repository.

`AGENTS.md.local` (untracked, present only on the maintainer's machine) holds the host-specific deployment process: how to reach the server, the release and deploy commands, and the ones that look right but fail. Read it before deploying, restarting llm-swap, or reindexing deepwiki - do not reconstruct those steps from this file.

## Hard Rules

Never:

- Edit `internal/schema/quack.gen.go` or anything under `frontend/src/generated/` - these are generated; changes will be overwritten and CI will fail.
- Add an unrecognised file to an `agents/<name>/` bundle. Each bundle contains exactly `agent-card.json` and `prompt.md`, plus the optional `rubric.yaml` (judge rubric) and `memory.md` ("what to remember" guidance, M6).
- Edit `openapi.yaml` without running `make generate` and committing the regenerated files.

Always:

- Run `make generate` after any change to `openapi.yaml` and include the generated files in the same commit.
- Run `go test ./...` and `cd frontend && npm test` before marking a task done.
- Run `make vet && make fmt` before committing Go changes.

## Talking to quack

The `quack` CLI is your primary interface for interacting with a running quack - creating chats, sending messages, inspecting runs, and controlling nodes mid-run. Reach for it before hand-rolling `curl` against the REST API. See [`docs/cli.md`](docs/cli.md) for the full command map.

```bash
quack chat new                       # create a chat, print its id
quack chat send <id> "<msg>"         # send a message (also answers a paused question)
quack chat show <id> -f              # status snapshot; -f follows a live run
quack chat list                      # chats and their status
quack chat node retry <id> <node>    # re-run a finished node and everything downstream
```

Exit codes make it scriptable: `0` answered, `1` failed, `2` paused on a question. `quack api [method] <path>` is a `gh api`-style passthrough when a command for what you need does not exist yet - if you find yourself using it often, that gap is worth filing.

## QA before opening a pull request

A feature is not ready for review until it has been exercised on a **non-production** quack server. Never QA against prod.

Work these in order, stopping when you have the answer:

1. **The UI**, driven through the Chrome MCP - it is the surface users actually touch, and it catches wiring and rendering faults nothing else does.
2. **The CLI** (see above) - drive chats and nodes directly, and check exit codes.
3. **OTel data** - traces and spans for what actually ran, how long it took, and where a run stalled.
4. **Logs** - last resort, for detail the first three could not give you.

Say in the PR what you exercised and what you saw. "Tests pass" is not QA: the crash in #1016 shipped through a green suite.

## Commands

```bash
# One-time after cloning: the plugin trees are not in git and embed.go embeds
# them, so a bare `go build`/`go test` fails until this has run.
make plugins

# Build (compiles frontend first, embeds dist into binary)
make build

# Run Go tests (make test runs `make plugins` first)
go test ./...

# One-time: internal/vetting's mermaid tests shell out to scripts/mermaid-validate.mjs.
# Without its deps they fail with "the mermaid validator produced unreadable output"
# (13 tests) rather than skipping - it degrades gracefully only when node or the
# script is absent, not when the imports fail to resolve. CI installs these too.
cd scripts && npm ci

# Run a single package's tests
go test ./internal/vetting/...

# Go vet + fmt
make vet
make fmt          # gofmt -w internal cmd

# Frontend dev server (hot-reload at :3000)
cd frontend && npm run dev

# Frontend tests (vitest)
cd frontend && npm test

# Storybook
cd frontend && npm run storybook

# Type-check + lint frontend
cd frontend && npx tsc --noEmit && npx eslint src/

# Regenerate Go server stubs + TypeScript client from openapi.yaml
make generate     # runs scripts/generate.sh (oapi-codegen + openapi-ts)

# Lint openapi.yaml
npx @redocly/cli@latest lint openapi.yaml --config redocly.yaml

# Full stack (app + Postgres + searxng) via Docker
make docker-up
make docker-down
```

CI checks: `go vet`, `deadcode` (`.github/workflows/ci.yaml`), `go test ./...`, `gofmt -l`, `tsc --noEmit`, `eslint`, `knip` (`npm run knip`), `npm run build`, `npm test`, OpenAPI lint, and a codegen-drift check (`git diff --exit-code -- internal/schema frontend/src/generated`).

## Architecture

### Source of truth: `openapi.yaml`

`openapi.yaml` is the single source of truth for the API contract. `make generate` (via `scripts/generate.sh`) derives two artifacts from it:

- **`internal/schema/quack.gen.go`** - Go chi-server stubs + request/response types (oapi-codegen, config: `internal/schema/cfg.yaml`)
- **`frontend/src/generated/`** - TypeScript client (openapi-ts, config: `frontend/openapi-ts.config.ts`)

### Go module

Module path: `github.com/fagerbergj/quack`. The binary entrypoint is `cmd/quack/main.go`.

### Request lifecycle

```text
HTTP request
  → internal/server/router.go   (chi router; registers generated REST routes + MCP mount)
  → internal/server/rest/       (REST handler; dispatches to orchestrator)
  → internal/orchestrator/      (Orchestrator.Run: one workflow/runner; plan → native ADK graph)
  → internal/dag/planner.go     (Planner: LLM plan → DAG) + nativegraph.go (RunPlanAsGraph)
  → each node → vetting.RunGatedRefine (internal/vetting/node.go - worker rounds + gate)
  → internal/vetting/judge.go   (independent judge model scores output)
  → internal/dag/executor.go    (dagStream: ADK session events → SSE vocabulary)
```

A GitHub webhook run enters differently: it's an SDK extension module (`quack-extensions/github`, outside this repo entirely), not part of `rest/` or the OpenAPI contract. Its route self-verifies via HMAC and is mounted on the public SDK-extension router (`router.go`); its dispatch calls `internal/serve.newExtDispatch` (`sdk.Host.Dispatch`), never `Orchestrator.Run` directly - see "GitHub extension" under Trust gate below.

### DAG execution (`internal/dag/`)

- `plan.go` - `Plan` and `Node` structs; `Node.DependsOn` encodes edges; `Plan.Setup` (repo/base/branch) declares the pre-provisioned clone.
- `planner.go` - one LLM call decomposes a request into a `Plan`, with a per-node acceptance rubric.
- `nativegraph.go`/`graph.go` - the plan runs as ONE native ADK workflow graph under one runner (`WithMaxConcurrency`); all nodes share one workflow session (id = chatID), isolated by branch + isolation scope. `buildGateNodes` wraps each node's worker in `vetting.RunGatedRefine` and wires the per-node `AdmissionSpec` (model sessions/kv_tokens/provider residency, from `models.<m>.limits` + `providers.<p>.limits.active`) into the shared `Admission` ledger (`admission.go`), gating every gated node on capacity before it starts. Continue-but-warn on gate-failed dependencies.
- `executor.go` - `dagStream` translates raw ADK session events (by `NodeInfo.Path` + `worker-rN` run ids) into the SSE vocabulary.

### Trust gate (`internal/vetting/`)

`RunGatedRefine` (node.go) runs the worker, then loops cheapest-first before the DAG propagates its output:

1. **Continuation** - mechanical completion signals (empty answer, undelivered commit, unposted review) hand the worker another tool-bearing round, up to 4.
2. **Deterministic checks** - citation backing, length, delivery/review/behaviour criteria, and `checksPassCriterion` (checks.go): the repo's own build/vet/test commands derived from the clone and run via `workspace.RunPipeline` (allowlist `workspace.check_commands`, default ON, toolchain-gated).
3. **Independent judge** (judge.go) - a separate model scores G-Eval style; weakest-link (lowest criterion), threshold default `0.7`. Judge/revise rounds re-prompt the worker with self-contained feedback.
4. **Adversarial skeptic pass** (adversarial.go) - inside the judge round, load-bearing criteria earn their score only after independent skeptics (`cfg.Skeptic`/`cfg.SkepticRounds`) try to refute them; a refuted criterion is downgraded before the round scores.

Ground-truth probes for external (ACP) workers: `augmentFromRepo` (gitprobe.go) reads commits/changed files off the clone and synthesizes the staged PR; `augmentFromAnswer` (answerreview.go) parses a reviewer's `VERDICT:`/`FINDINGS:` tail into the staged review with inline comments. Delivery is gate-owned (`commitDelivery` → the GitHub extension), fires exactly once, and a gate-failed PR opens as a draft. `commitDelivery` also partitions staged items against the run's `vetting.Config.AllowedDeliveryKinds` (nil for a non-GitHub run, which permits everything) before they reach the extension - an ungranted item is refused, logged at Error, and reported as a failed `delivery_result`, never silently dropped or silently shipped.

### GitHub extension (`quack-extensions/github` - a separate repo, see below)

The GitHub App integration is an SDK extension module living in `github.com/fagerbergj/quack-extensions/github`, not in this repo. quack only: blank-imports it (`internal/serve/extensions_registry.go`), pins it in `go.mod`, wires its four Host capabilities (`EnsureContextDir`, `ChatUser`, `ArchiveChat`, `Classify` - `internal/serve/extensions.go`'s `buildSDKExtensions`), and detects it as the `sdk.Deliverer`/`sdk.GitCredentialSource` implementation (`findDeliverer`/`findGitCredentialSource`, adapted to `vetting.DeliverFunc`/`tools.GitTokenSource` in `serve.go`) - the same type-assertion pattern `Starter`/`Stopper`/`sdk.UI` use, not hardcoded to one extension's name. Its config lives entirely under `extensions.github:` in `quack.yaml`, parsed by the module's own `Factory`, opaque to quack.

A GitHub webhook dispatch computes the allowed-kinds list once, from the labels currently on the issue/PR, PR authorship, and a fork check (`computeGrant`), then builds a structured envelope: `<permissions>`, `<deliverable>`, hoisted `<issue>`/`<pull_request>` (title + description), `<comments>` (full on first load, new/edited/deleted delta on resume), `<changed_files>` on a PR, the triggering `<event>` (GitHub's own webhook JSON filtered by a fixed drop-list - `node_id`, every `*_url`, `avatar_url`, `reactions` - never renamed or reshaped), and a `<context dir>` pointer (written via `Host.EnsureContextDir`, holding the untruncated GitHub API responses the envelope only summarizes - `issue.json`, `comments.json`, `pull.json`, `files.json`, `commits.json`, `reviews.json`, `review-comments.json`, `check-runs.json`/`annotations-*.json`, `linked-issue-*.json`, `timeline.json`; sandboxed children get it mounted read-only via `Caps.ExtraRO`). It calls `Host.Dispatch` once per turn (fire-and-forget - `sdk.Host.Dispatch` never drives a run to completion inline) and implements `sdk.RunObserver.RunEnded` to finish the job: a label-triggered dispatch whose outcome carries `PlanRan: false` gets exactly one nudge follow-up (a second `Dispatch` call, not a raw event stream); a verified delivery (`sdk.Deliverer.Deliver`, gate-owned push already done) skips the duplicate summary comment; otherwise the run's answer (or a HITL question, on `RunNeedsInput`) is posted as a comment. The four GitHub-specific state tables (snapshot/review-baseline/fix-state/merge-intent) live in the extension's own SQLite file under `Host.DataDir`, not quack's Postgres; a `keyedMutex` serializes the merge-intent read-verdict-act sequence per chat, since SQLite gives up the incidental ordering Postgres's shared connection used to provide.

### Agents: external ACP subprocesses + native bundles (`internal/acp/`, `internal/agent/`, `agents/`)

ALL code agents (code-implementer, code-reviewer, code-explorer) run as EXTERNAL subprocesses speaking the Agent Client Protocol (`internal/acp`) - the `tools/pi-acp` shim driving pi by default, spawned per worker round, model bound via generated `OPENCODE_CONFIG_CONTENT` (which the shim parses), `git push` denied, quack's skill library injected via that env's `skills.paths`. They have NO quack tools; the gate's probes read their work off the clone/answer. Configured per agent with `acp: {command, env, read_only}`.

Native (llmagent) bundles remain for the non-code agents (web-researcher, synthesizer, media/image readers, memory-agent, advisor, orchestrator):

```text
agents/<name>/
  agent-card.json   # A2A AgentCard: identity + skills
  prompt.md         # system prompt (for ACP agents: the per-round preamble)
  rubric.yaml       # optional: per-agent judge rubric (falls back to config/rubric.md)
  memory.md         # optional: "what to remember" guidance (M6), native agents only
```

`agent.LoadBundle` reads the bundle; `agent.Build` turns a native one into an LLM agent. The config (`config/quack.yaml`) binds a model (and, for native agents, a tool list) to each bundle.

Before changing anything under `internal/workspace/`'s sandbox (`sandbox.go`, `jail.go`, `exec.go`) or an ACP agent's env/argv wrapping (`internal/acp/proc.go`, `internal/serve/serve.go`'s `opencodeEnv`), run `quack sandbox check --agent code-reviewer` first (see `docs/sandbox-cli.md`) - it's the same `Caps`/`WrapArgv`/`spawnEnv` path the ACP child gets, and it catches a regression before the next review does.

### Inference (`internal/inference/`)

`inference.NewModel(providerConfig, modelName)` is the single factory for `model.LLM`. Only `kind: "openai"` is implemented (any OpenAI-compatible endpoint). Adding a new provider kind is localized to `factory.go`.

### Streaming (`internal/stream/`)

`stream.SSEEvent` is the wire-level vocabulary shared by REST, MCP, and A2A. The key event sequence within a node:

```text
dag_plan → node_queued → node_start
  → agent_start (stage: worker)
  → agent_thinking / agent_tool_call / agent_tool_result / agent_token
  → agent_complete
  → [agent_start (stage: judge) … agent_complete]
  → [agent_start (stage: revise) … agent_complete]    (on judge fail, loops back to judge)
  → node_done (or node_failed)
done
```

`stream.Translator` converts raw ADK session events into this vocabulary. The `stage` field on `agent_start`/`agent_complete` (`worker`, `judge`, `revise`) lets the frontend group runs inside a node. A worker's `ask_advisor` consults (internal/tools/ask_advisor.go) are NOT a separate stage - they surface as ordinary `agent_tool_call`/`agent_tool_result` activity within the worker's own run.

### HTTP server (`internal/server/`)

- `router.go` - mounts MCP at `/api/v1/mcp`, registers generated chi routes, serves the embedded SPA for everything else.
- `rest/` - concrete HTTP handler that implements the generated `StrictServerInterface`.
- `mcp/` - MCP Streamable-HTTP server.
- The SPA (`frontend/dist`) is embedded into `internal/serve/web/dist` at build time (`make frontend-build`).

### Frontend (`frontend/`)

React 19 + Vite + Tailwind CSS 4 + TanStack Query. State is in `src/state/`:

- `chatStore.ts` - Zustand-like store for chat sessions and messages.
- `agentStream.ts` - parses the SSE event stream and updates the store.
- `ChatStoreProvider.tsx` - context provider.

Components under `src/components/` have co-located Storybook stories (`.stories.tsx`) and vitest tests (`.test.ts`). Tests stub `global.fetch` directly with `vi.fn()`/`vi.stubGlobal`, not a mocking library.

### Stores

- **Postgres** (GORM, `gorm.io/driver/postgres`) - ADK sessions + events, DAG plan/node state, chat metadata. Connection via env `QUACK_DATABASE_URL`.
- **qdrant** - semantic memory / RAG vectors. Connection via env `QUACK_QDRANT_URL`.

### Key env vars

| Var | Purpose |
|-----|---------|
| `QUACK_LLM_ENDPOINT` | OpenAI-compatible LLM endpoint (e.g. `http://jason-server:11436/v1`); interpolated into `providers.default.endpoint` |
| `QUACK_LLM_API_KEY` | API key |
| `QUACK_ORCH_MODEL` / `QUACK_RESEARCHER_MODEL` / `QUACK_CODER_MODEL` / `QUACK_JUDGE_MODEL` | Per-role model names (`QUACK_CODER_MODEL` falls back to `QUACK_RESEARCHER_MODEL` if unset — the only chained fallback; every other agent's model, and the judge, is a hard startup error if unset while the agent/judge is enabled) |
| `QUACK_EMBED_MODEL` | Embedding model for the vector store |
| `QUACK_COMPACTION_ENABLED` / `QUACK_COMPACTION_MODEL` | Toggle + model for history compaction |
| `QUACK_DATABASE_URL` | Postgres DSN |
| `QUACK_QDRANT_URL` | qdrant endpoint (unset ⇒ memory self-disables) |
| `QUACK_SEARXNG_URL` | SearXNG JSON API endpoint for web search |
| `QUACK_WORKSPACE_ROOT` | Filesystem sandbox root (default `./workspace`) |
| `QUACK_LOG_LEVEL` | slog level: `debug`, `info` (default), `warn`, `error`. `debug` surfaces per-round hot-path trace (vetting/compaction). |
| `QUACK_LOG_FORMAT` | slog output: `text` (default, human-readable) or `json` (one object per line, for log aggregators). |

## Comments

Comments say what the CODE CANNOT - the incident belongs in the commit message and PR body, not the source.

- Keep: non-obvious constraints and invariants, the ceiling of a deliberate shortcut, warnings that stop the next person breaking something, `ponytail:` markers.
- Cut: incident narratives (dates, node ids, token counts, quoted model output), what the code plainly does, how we got here, rejected alternatives.
- A load-bearing war story is ONE clause (`// (a live grep returned 48 MB - bound the bytes, not just the count)`), never a retelling.
- If one line of code needs 3+ lines of comment, the comment is too long. Test files may keep a short failure narrative (they pin regressions) - a few lines, not twenty.

## Spec-Driven Development

Before implementing a non-trivial feature, write a brief spec covering:

- **Scope** - what is in and explicitly out of scope
- **Forbidden actions** - what the implementation must never do
- **Available tools / interfaces** - what it can call or depend on
- **Output contract** - shape and format of what it produces
- **Test cases** - at least 2–3 concrete input/output examples

Keep the spec in the PR description or a `docs/` file. Any behavioral drift from the spec becomes a failing test rather than a production incident.
