# Slots extension

Status: draft, 2026-11-01. Core terms are in the [SRS glossary](../srs.md#1-glossary).

## Product

**Job.** Keep a model host from interleaving more runs than it serves at once (D60). llm-swap's 27B serves 6 sequences; past that vLLM queues requests itself, sessions' turns interleave, and each session's prefix leaves the prefix cache while it runs tools. A slot held for the whole run keeps its prefix warm.

**Success.** With more active runs than slots, no run's turns re-prefill the whole context because others ran between them, and no slot outlives its holder.

## Requirements

| ID | Level | Requirement | Trace |
| --- | --- | --- | --- |
| SLT-1 | MUST | Hold N slots per model endpoint, N set in deployment config; queue waiters first in, first out. | D60 |
| SLT-2 | MUST | A run takes a slot before its first model request and keeps it between its own turns. | D60 |
| SLT-3 | MUST | Release the slot when the run ends, when the run waits on a person (approval or decision), and after an idle grace (default 60 s) with no model request in flight, which covers every other wait: a child run, a workflow step that calls another session. A run needing its slot while others wait cannot deadlock the cap. | D60 |
| SLT-4 | MUST | A run holding a slot past a cap (default 10 min) requeues behind waiters at its next request. | D60 |
| SLT-5 | MUST | Slots are leases kept by heartbeat: a crashed session frees its slot within the lease TTL, and a restarted session takes its old place back rather than holding two. | D60 |
| SLT-6 | MUST | Fail open: if the service is unreachable or unconfigured, requests proceed ungated and one warning is logged. A fresh install with only `QUACK_DATA_DIR` runs unchanged. | D60 |
| SLT-7 | MUST | A queued session's status line reads "Waiting for a model slot (n ahead)" in the session list; the service records each wait's duration. | D60 |

## Not now

Residency (never admit a second model while the first holds a single-resident host) and aging (stop backfilling past the oldest waiter after 2 minutes). v0 #1007 had both; the gate here is a number of runs per endpoint, FIFO.

## Configuration

| Variable | Half | Meaning |
| --- | --- | --- |
| `QUACK_SLOTS_LIMITS` | service | JSON of endpoint (provider id) to slots, for example `{"llm-swap":6}`. An endpoint not listed is not gated. |
| `QUACK_SLOTS_TOKEN` | both | Shared bearer token (A12 until workload tokens land). |
| `QUACK_SLOTS_DB` | service | SQLite file, default `/data/slots.db`. |
| `PORT` | service | Default 8092. |
| `QUACK_SLOTS_URL` | session | The service's base URL. Unset: the gate is off. |

The deployment enables the gate for every session, whatever its agent selects: a provider limit only works if every session obeys it. Setting `QUACK_SLOTS_URL` and `QUACK_SLOTS_TOKEN` turns it on; leaving either unset turns it off.
