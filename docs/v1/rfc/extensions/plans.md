# Plans extension architecture

- **Status:** Accepted, 2026-10-30
- **Requirements:** [plans](../../extensions/plans.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

Plans gives a planner role the tools to split a large project into parallel child sessions, each with its own agent and role, and to track and assemble their results. It's opt-in and not used by any launch workload.

## Design

- **The plan** is a pi-durable document in the planner's session: parts, assigned agent and role, child session key, status, result reference. The UI renders it as the plan view (PLN-3).
- **Tools:**
  - `plan_spawn` starts a child session through the distributor (D34).
  - `plan_message` sends input to a child or steers it.
  - `plan_stop` stops a child.
  - `plan_wait` waits durably for children.
  
  A child's final artifact returns to the planner as a follow-up input carrying the sealed version reference.
- **Gating.** Each child runs under its own agent definition, gates and checks. The planner's assembled artifact is checked like any other (PLN-4).

## Unresolved questions

None beyond the core RFC.
