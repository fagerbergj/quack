# Memory extension architecture

- **Status:** Accepted, 2026-10-30
- **Requirements:** [memory](../../extensions/memory.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

The session half lets agents propose memories and read approved ones. The service half keeps the memory store and its editing page. Nothing is stored without the operator's approval.

## Design

**Session half:**

- **`memory_propose`:** a tool that is always gated, with a chain ending at the operator (D7). An extension-mandated gate can't be removed by a definition. On approval, the call writes the memory through the service half.
- **A prompt section** renders the pinned memories that match the session's scope (global, agent, subject) in a stable order, so the prompt cache stays warm (MEM1).
- **`memory_recall`:** searches the rest.

**Service half:**

- Keeps the memory store: scoped memories with their provenance (the session and entry that proposed them).
- Serves the page where the operator lists, edits and deletes memories.

**Benchmark.** A definition flag turns memory off, so the eval extension can run an agent's benchmark with and without memory. Memory stays on only for agents where it wins (MEM2).

## Unresolved questions

None beyond the core RFC.
