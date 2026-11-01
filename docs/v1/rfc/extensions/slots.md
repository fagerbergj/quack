# Slots extension architecture

- **Status:** Accepted, 2026-11-01
- **Requirements:** [slots](../../extensions/slots.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

A service half keeps a lease ledger of slots per model endpoint; a session half is a pi-durable `GenerationTask` hook that takes a slot before each request and gives it back by rules a fake clock can test.

## Design

- **Ledger.** One SQLite row per holder or waiter per endpoint. Each call first deletes expired rows, then promotes waiters in arrival order while slots are free, so a crashed holder frees its slot by the clock alone and the service runs no timers. `acquire` is idempotent per holder: asking again refreshes the lease and returns the place (SLT-5). A waiter that stops polling expires like a holder. Each grant records the wait's duration (SLT-7).
- **Holder and endpoint.** The holder is the session key, so one session process is one holder and its decision forks share its slot. The endpoint is the provider id of the agent's model, read from `AgentDoc` in `beforeRequest`.
- **Acquire.** `beforeRequest` blocks on the gate: it polls `acquire` (1 s) until granted and sets the status note "Waiting for a model slot (n ahead)". Polling keeps the service stateless and the client testable on a fake clock.
- **Release.** `afterResponse` starts the idle grace; the next `beforeRequest` cancels it. `onYield` and the SDK's waiting signal (below) release at once when nothing is in flight. A backstop timer armed at each request releases a hold whose `afterResponse` never came (a failed request).
- **Why the grace is the deadlock rule.** A parent run waiting on child runs holds a slot while generating nothing. Anything the extension cannot name as "a person" is covered by the grace, so a parent holding one of N slots while N children queue yields its slot within the grace and the children proceed. Tested with N=6 and 6 children.
- **Cap.** At `beforeRequest`, a hold older than the cap is released and asked for again, which puts it behind any waiter and costs nothing when none waits (SLT-4).
- **Fail-open.** Any error from the service marks it down for 30 s, logs one warning, and lets requests through ungated. Heartbeat errors are ignored; a heartbeat answered "not held" drops the local hold so the next request asks again (SLT-6).

## What the contract had to grow

Two small, general additions to `@quack/sdk`, neither slots-specific:

1. **`whileWaiting` / `onWaiting`.** Hooks see requests and tools but not that a tool is blocked on a person. `gated` and workflow decisions wrap their wait in `whileWaiting`; any session extension can subscribe. Without it the person-wait case would fall back to the grace.
2. **`statusNote`.** A session reports an extension's one-line status through `/ping` (the distributor already pings every running session and lists sessions from that), and the session list shows it ahead of the generic "running". The distributor stores the latest note per session; no new screen or endpoint.

Everything else used what pi-durable provides: `GenerationTask` hooks (`beforeRequest`, `afterResponse`, `onYield`), `AgentDoc`, `ctx.abortSignal`. pi-durable's own retry and busy-conversation handling are untouched; the gate does not queue or retry requests, it only delays the first byte.

## Unresolved questions

- Run end is seen only through `onYield` and the grace; the harness's `run_end` event is available to hosts, not extensions.
- A killed session shows a stale note until it next acquires.
