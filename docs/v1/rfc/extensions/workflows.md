# Workflows extension architecture

- **Status:** Accepted, 2026-10-30
- **Requirements:** [workflows](../../extensions/workflows.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

A workflow is a pi-durable task whose phases are the workflow's steps. Each step commits its result before the next one starts, so a crash resumes at the interrupted step. Extensions define workflows in code, and an agent definition binds a workflow to a role, so a run in that role executes the workflow instead of the free agent loop.

## Design

- **`defineWorkflow`** takes named steps of four kinds:
  - code
  - a model call with an output schema
  - a decision point
  - a gated action
  
  Each step reads earlier steps' results from the task checkpoint.
- **Durability.** One task phase per step, with the step's result committed as the next phase's checkpoint (WFL-2). A step that calls the outside world must be idempotent or keyed, because a phase can rerun after a crash.
- **Visibility.** A `quack.workflow` document records each step's status, duration and cost, and the session view renders it (WFL-4). Artifacts written by steps are sealed and checked like any other (WFL-3).

## Unresolved questions

None beyond the core RFC.
