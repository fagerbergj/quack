# Observability

Quack emits traces and metrics via OTel (`internal/otelobs`) but keeps no local store or read API of its own — Tempo/Grafana (or whatever your OTLP collector feeds) own viewing. Emission and export are two separate knobs:

```yaml
observability:
  otel:
    enabled: ${QUACK_OTEL_ENABLED}             # unset ⇒ true — spans/metrics/logs are recorded either way
    otlp_endpoint: ${QUACK_OTEL_OTLP_ENDPOINT} # unset ⇒ nothing exported (harmless — just inert)
    sample: 1.0                                # trace sample ratio in (0,1]
    environment: ${QUACK_OTEL_ENVIRONMENT}     # unset ⇒ production
```

Every signal carries a resource naming the build and the deployment: `service.name`, `service.version` plus `langfuse.release` (both the version `cmd/quack` stamps into `serve.Version`, omitted entirely on a dev build that has none), and `deployment.environment.name` from `otel.environment`. That last one is how a laptop run stays out of the deployed server's traces — set `QUACK_OTEL_ENVIRONMENT=development` locally.

`enabled: false` swaps in the SDK's no-op providers, so every `otelobs.Start`/`Record*` call in the code stays a cheap no-op with no `if enabled` branch at any call site. `otlp_endpoint` unset means spans are still recorded and metrics still accumulate in-process — they're just never shipped anywhere. Set it to actually export (e.g. `http://otel-collector:4318`).

**ADK's own spans come along for free.** ADK v2 takes its tracer from the *global* provider at package-init time (`otel.GetTracerProvider().Tracer("gcp.vertex.agent", …)`), and OpenTelemetry's global package exists precisely to survive that ordering: `otel.SetTracerProvider` walks every tracer handed out earlier and rebinds it to the real provider. So ADK's instrumentation lands in the same OTLP stream as quack's, correlated inside the same trace — a single run carries both:

```text
scope=github.com/fagerbergj/quack   quack.run
scope=gcp.vertex.agent              invoke_workflow orchestrator-workflow
                                    invoke_agent orchestrator
                                    generate_content qwen3.6-35b
```

ADK's spans carry GenAI semantic-convention attributes (model, token usage), so they're worth querying by their `gcp.vertex.agent` scope rather than filtering to `quack.*` only. ADK emits **spans only** — it registers no metric instruments, so every metric below is quack's own. (ADK also ships a `telemetry/setup_otel.go` helper that builds its *own* SDK provider; quack does not use it, which is what keeps everything on one provider.)

## Traces

Quack's own spans are named `quack.<name>` (ADK's, above, are not). The vocabulary:

| Span | Covers |
| --- | --- |
| `quack.run` | One orchestrator run, start to finish. |
| `quack.node` | One DAG node (paired with `quack.nodes.active` — see below). |
| `quack.worker.round` | One worker round within a node's trust gate — draft, continuation, revise, HITL, or confirm. |
| `quack.gate.checks` | The deterministic-checks stage. |
| `quack.plan` / `quack.plan.judge` | DAG planning and the plan judge's pass over it. |
| `quack.setup.clone` | Repo provisioning for a plan's `Setup` (the pre-provisioned clone). |
| `quack.delivery` | The gate-owned delivery step (commit/push/PR/review). |
| `quack.acp.spawn` / `.handshake` / `.prompt` / `.round` | The external ACP subprocess lifecycle (the `pi-acp` shim driving pi) for code-implementer/reviewer/explorer nodes. |
| `quack.acp.tool.<kind>` | One tool call the ACP subprocess made, from its `session/update` to its terminal status — a child of `quack.acp.prompt`. `<kind>` is the ACP tool kind (`execute`, `read`, `edit`, `search`, …). |

Quack's spans wrap work, not model calls — the model calls are ADK's `generate_content` spans nested inside them. So a quack span never carries `model` or `gen_ai.request.model`: OTel consumers (Langfuse among them) read a model-named attribute as "this is a model call" and would fold node and round wall-clock into every per-model latency and cost aggregate. Where the model a wrapper ran under is still worth having (`quack.node`, `quack.worker.round`), it goes under `quack.model`.

An ACP round can run for two hours, and a span only exports when it ends — so `quack.acp.round` and `quack.acp.prompt` tell you nothing until the round is over. `quack.acp.tool.<kind>` is what makes a running round visible: one span per tool call, ended as the update is handled, so the last one's end time is the round's last sign of life. Tool granularity is deliberate — thought and message chunks arrive per token (thousands per review), tool calls in the tens to low hundreds. The subprocess's own model calls go straight to llm-swap without trace context, so they appear in neither this trace nor as links.

A fire-and-forget span (e.g. the async memory commit) uses `StartLinked` instead of a child span — it gets its own root, linked to (not nested under) the run that triggered it, since the run may finish and close its own span before the commit does.

`TraceIDOf(ctx)` surfaces the active trace id for cross-referencing a durable event-log line (e.g. `delivery_result`) into Tempo by hand.

## Metrics

All under the `quack.*` namespace (`internal/otelobs/metrics.go`), meter name `github.com/fagerbergj/quack`:

| Metric | Type | Attributes | What it's for |
| --- | --- | --- | --- |
| `quack.runs.active` | UpDownCounter | — | Orchestrator runs currently holding a concurrency slot. |
| `quack.runs.queued` | UpDownCounter | — | Runs admitted but waiting on a server-wide run slot (unbounded by default since #1007; nodes queue on model/provider capacity instead). |
| `quack.nodes.active` | UpDownCounter | — | DAG nodes currently in flight. |
| `quack.worker.round.duration` | Histogram (s) | `agent`, `model`, `stage` (`worker`/`judge`/`revise`) | Per-round wall time, drawn from the same window as `quack.worker.round`'s span so the two can never disagree. |
| `quack.judge.score` | Histogram (0–1) | `agent` | The weakest-link score from every judge round, any agent. |
| `quack.judge.verdict` | Counter | `agent`, `passed` | Pass/fail counts per agent. |
| `quack.judge.unavailable` | Counter | `agent` | A judge round that errored before producing a verdict — explains a gap in `judge.score`/`judge.verdict` for that agent. |
| `quack.delivery.outcome` | Counter | `outcome` (`delivered`/`draft`/`failed`/`none`) | `none` is the alertable one: a judge-passed work-request that recorded no delivery attempt at all — the phantom-success class this metric exists to catch. |
| `quack.model.call.duration` | Histogram (s) | `model` | Raw model call latency — swap-sensitive on a shared local backend. |
| `quack.acp.permission_ask` | Counter | `agent` | ACP subprocess permission asks reaching the safety judge; expected to stay ~0 since every known ask class is answered in config. |
| `quack.memory.recall` | Counter | `hit` | Memory recall attempts, hit vs. miss. |
| `quack.gate.checks.skipped` | Counter | `reason` | A node whose deterministic checks did NOT run at all (no build/vet/test backstop) — the gate then relied on judge score alone. Query this to find nodes gated on judge alone. |
| `quack.memory.commit.failures` | Counter | `agent`, `reason` | A fire-and-forget memory commit that errored (consolidation-model timeout, embed/neighbour timeout) — the only queryable signal for the commit goroutine, which never fails a node itself. |

`quack.delivery.outcome=none`, `quack.gate.checks.skipped`, and `quack.judge.unavailable` are the three "silent gap" signals worth alerting on directly — each exists specifically because quack has shipped a real incident where the corresponding failure mode looked identical to success until someone went looking (a fabricated exploration scoring high, a phantom delivery, a judge round dying without a verdict).

The active/queued/in-flight gauges (`quack.runs.active`, `quack.runs.queued`, `quack.nodes.active`) don't survive a hard process kill — the process that incremented one never runs its matching decrement, so a restart orphans the gauge high until the new process starts fresh at 0. Treat them as advisory around a deploy; the durable event log and Tempo traces are the source of truth for what was actually in flight.

## Logs

`internal/otelobs/sloghandler.go` bridges `log/slog` to trace correlation — see [`AGENTS.md`](../../AGENTS.md)'s `QUACK_LOG_LEVEL`/`QUACK_LOG_FORMAT` for the logging side of this. Separately, `internal/otelobs` also runs an OTel *logger* provider (`internal/otelobs/logs.go`) — this is the replay ledger's transport, not `slog`.

## Replay ledger

Three seams emit one `gen_ai.*` OTel log event per call — `inference.NewModel` (`chat`), `tools.Build` (`execute_tool`), and the ACP subprocess connection (`invoke_agent`, the full teed protocol conversation) — plus the judge emits one `gen_ai.evaluation.result` per rubric criterion. Every event carries `gen_ai.conversation.id` (the chat id) and the two custom `quack.node`/`quack.round` attributes, stamped once by the vetting gate and read back via `internal/ledger`'s context carrier.

```yaml
stores:
  default_ledger:
    kind: filesystem              # append-only session recordings; s3 is a future adapter
    root: ${QUACK_RECORDING_DIR}  # unset ⇒ ./recordings

observability:
  recording:
    enabled: ${QUACK_RECORDING_ENABLED}  # unset ⇒ follows otel.enabled
    store: default_ledger
    retention_days: 30
```

The built-in ledger exporter appends every event as one redacted (auth headers/API keys/credentials stripped) JSON line to `recordings/<chat-id>.jsonl` — quack's default "collector". Recording rides the SAME logger provider as `otlp_endpoint`, so it can only be on when `otel.enabled` is; a store that fails to resolve degrades to "not recording" (a Warn log), never a startup failure or a change to the run itself. Export and replay (reconstructing/replaying a run from these recordings) are later milestones — this stage is emission + storage only.
