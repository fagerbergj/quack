# Approvals extension architecture

- **Status:** Accepted, 2026-10-30
- **Requirements:** [approvals](../../extensions/approvals.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

The quack SDK owns the gate mechanism: block, wait durably, resume, return a rejection, audit. This extension owns the policy. It reads each gated action's approval chain from the agent definition, runs it as a sequence of decision points, and provides the human resolver.

## Design

- **Chain execution.** A chain is an ordered list of resolvers, such as a code check, the judge's verdict on the referenced artifact version, a named model, or a human. Each resolver runs as a decision point with approve, reject and escalate options. The first approve or reject ends the chain, and an escalate passes to the next resolver (D16).
- **No chain configured.** The action proceeds without waiting (D41). An extension can mark an action as always gated with a minimum chain, as memory does, and a definition can't remove that.
- **The human resolver:**
  - Writes a pending approval that the session publishes to the index, so it appears in the inbox.
  - Triggers a web push.
  - Waits durably, for days if needed.
  - Takes the verdict back as a submission from a participant with the approver role (D39).
  
  Self-approval is allowed (D38).
- **Audit.** Every step of a chain, with its resolver, evidence and outcome, is a log entry and an audit fact (GOV-3).

## Unresolved questions

None beyond the core RFC.
