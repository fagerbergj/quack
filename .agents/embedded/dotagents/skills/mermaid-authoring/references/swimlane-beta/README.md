# Swimlanes

- **Keyword(s):** `swimlane-beta`
- **Introduced:** mermaid v11.16.0+. **Beta** — the doc's own warning: "This is a new diagram type in Mermaid. Its syntax may evolve in future versions." Very new; expect GitHub and other pinned renderers to show nothing for this block.
- **Use when:** a process where "who owns this step?" matters as much as "what happens next?" — approvals, support escalation, cross-team handoffs.
- **Avoid when:** ownership isn't the point — use `flowchart` for plain sequence/branching, `sequenceDiagram` for message timing between participants, `stateDiagram` for one thing's state changes.

## Minimal example

```mermaid
swimlane-beta LR
  subgraph Customer
    request[Request service]
  end

  subgraph Support
    triage[Triage request]
  end

  request --> triage
```

## Core syntax

Starts with `swimlane-beta`, optionally followed by a direction: `TB` (default), `TD` (same as `TB`), `BT`, `LR`, `RL`.

### Lanes

`subgraph` at the top level becomes a lane; it ends with `end`. Give it a stable id plus a display label with `subgraph id [Label]` — useful when the label has spaces or you want to style/reference the lane later.

```text
subgraph sales [Sales team]
  lead[Qualify lead]
end
```

### Nodes and edges

Both use **flowchart syntax verbatim** — same shape delimiters (`[]`, `()`, `([])`, `{}`, `(())`), same edge forms (`-->`, `---`, `-->|label|`, `-.->`,`==>`). Edges cross lane boundaries freely; a cross-lane edge is a handoff. Full shape/edge catalog: the [flowchart](../flowchart/README.md) doc — swimlanes intentionally doesn't redefine it.

### Accessibility

`accTitle:` and `accDescr:` right after the direction line, same convention as other mermaid diagrams.

## Gotchas

- Only **top-level** `subgraph` blocks become lanes — this diagram type does not define nested-lane semantics beyond what flowchart's nested subgraphs already do, so keep lanes flat.
- The rendered examples in the official doc use a specific look/theme (`Neo` + `Redux`) for readability — your default theme will look different unless you configure it the same way; that's cosmetic, not a syntax issue.
- Being brand-new (v11.16.0), don't assume any given deployment's mermaid is recent enough — check the pinned version before relying on this diagram type rendering at all.

## Deeper

- [examples.md](../../assets/swimlane-beta/examples.md) — approval, escalation, and fulfillment flows
