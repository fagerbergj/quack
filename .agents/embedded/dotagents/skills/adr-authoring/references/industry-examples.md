# ADR industry examples

Use these examples to choose the smallest structure that preserves a decision’s rationale. A house template, status vocabulary, numbering rule, and mutation policy take precedence.

## Document shapes

### Michael Nygard’s minimal ADR

**Fits:** Most bounded decisions where context, decision, status, and consequences tell the complete story.

**Published shape:**

- Title
- Context
- Decision
- Status
- Consequences

The record is short, focused on one decision, stored with the project, and retained when later decisions supersede it.

Source: [Documenting Architecture Decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)

### MADR

**Fits:** Decisions whose rationale benefits from explicit drivers and option comparison.

**Published full shape includes:**

- Context and problem statement
- Decision drivers
- Considered options
- Decision outcome
- Consequences
- Confirmation
- Optional metadata for decision-makers, consulted people, and informed people

MADR also publishes a minimal variant. Use the full form only when the added sections carry real information.

Sources: [MADR](https://adr.github.io/madr/), [MADR template](https://github.com/adr/madr/blob/main/template/adr-template.md), [MADR primer](https://ozimmer.ch/practices/2022/11/22/MADRTemplatePrimer.html)

### Y-Statement

**Fits:** A compact decision that can be expressed without losing the central trade-off.

**Published form:** In the context of a use case, facing a concern, the team decided for an option to achieve a quality, accepting a downside.

Use it as a concise summary or small record, not to compress away context that future readers need.

Source: [Sustainable Architectural Decisions](https://www.infoq.com/articles/sustainable-architectural-design-decisions/)

## Lifecycle practices

### Nygard and Fowler

Nygard uses Proposed, Accepted, Deprecated, and Superseded. Fowler recommends preserving accepted ADRs and superseding rather than reopening them; he also recommends recording confidence when useful. These are influential defaults, not a universal mandate.

Sources: [Documenting Architecture Decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions), [Architecture Decision Record](https://martinfowler.com/bliki/ArchitectureDecisionRecord.html)

### AWS Prescriptive Guidance

AWS describes Proposed, Accepted, Rejected, and Superseded states. Review happens while the record is Proposed; accepted ADRs are immutable in that process, and a later change is recorded in a new ADR.

Use this when a team needs explicit approval and rejection without separate Discussion and Comment states.

Source: [AWS ADR process](https://docs.aws.amazon.com/prescriptive-guidance/latest/architectural-decision-records/adr-process.html)

### More granular local lifecycles

willhaben separates Open, In Progress, RFC, and Decided. Decentraland uses Draft, Review, Living, LastCall, and Final among other states. These are local governance designs, useful when a team genuinely needs separate exploration, review, final-comment, or living-document phases.

Sources: [willhaben ADR learnings](https://tech.willhaben.at/8-learnings-from-using-architecture-decision-records-adrs-at-willhaben-5b1594ebaffe), [Decentraland ADR process](https://adr.decentraland.org/adr/ADR-1)
