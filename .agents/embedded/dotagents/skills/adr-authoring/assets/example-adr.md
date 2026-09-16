# Real ADR example: Drop the Kafka dependency

- **Project:** Dependency-Track
- **Author:** nscuro
- **Status:** Accepted on 2025-01-07
- **Canonical document:** [Dependency-Track ADR 001](https://github.com/DependencyTrack/dependency-track/blob/main/docs/adr/001-drop-kafka-dependency.md)

This is a curated excerpt and reading guide. Dependency-Track’s own ADR guide identifies ADR 001 as its canonical example for comparing real alternatives.

> **Caution:** The accepted source still says “We propose” and contains `TODO: Update with final decision`. Its analysis is exemplary; its unfinished Decision section is not. Do not copy that defect.

## Why this example is useful

The ADR begins with how Kafka is actually used, then documents operational limitations observed by the project. It compares three credible directions with concrete pros and cons before selecting PostgreSQL. The consequences name migration work and compatibility obligations rather than presenting the choice as free.

## Published structure

- Status, date, and authors
- Context
  - Current use
  - Issues and limitations
  - Possible solutions with pros and cons
- Decision
- Consequences

## Excerpt

> In summary, *Kafka on its own provides not enough benefit for us to justify its usage*.

The ADR then evaluates replacing Kafka with another broker, using an in-memory data grid, and using PostgreSQL before selecting the PostgreSQL direction.

## What to learn from it

- Start from observed system behavior, not a generic technology preference.
- Give alternatives enough detail that the chosen option is understandable.
- Tie the decision to team capacity and support burden as well as technical qualities.
- Admit the chosen option’s scaling ceiling.
- Record migration and adopter impact as consequences.

Read the [complete ADR](https://github.com/DependencyTrack/dependency-track/blob/main/docs/adr/001-drop-kafka-dependency.md) before using it as a model.
