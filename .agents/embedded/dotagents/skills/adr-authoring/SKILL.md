---
name: adr-authoring
description: >
  Helps author and review Architecture Decision Records that preserve one significant technical
  decision, its context, status, and consequences. Use when a bounded decision is made or needs a
  lightweight durable record, especially alongside source code.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0"
---

# ADR authoring

## Overview

Use an ADR to preserve why one architectural or technical decision was made. Keep it focused on one decision and useful to someone encountering the system later. Follow the repository’s template, status vocabulary, numbering, and approval rules before applying this skill’s defaults.

## When to Use

Use an ADR when a decision is architecturally significant, its rationale would be costly to rediscover, and the goal is a durable record rather than an open-ended review. An ADR may begin as Proposed if the house process reviews decisions through ADRs.

## When NOT to Use

- Use an RFD when the problem or decision space is still exploratory.
- Use an RFC when a substantial proposal needs broad review before commitment.
- Use code comments, tests, or ordinary documentation for local implementation facts that are already obvious from the code.
- Do not create an ADR for every low-cost or easily reversible choice.

## Authoring Procedure

1. **Find the house standard.** Search for an ADR directory, template, index, numbering rules, statuses, ownership, and supersession practice. Follow them when present.
2. **Confirm significance.** State the decision boundary, why the rationale matters later, and whether an RFD or RFC should happen first.
3. **Gather the record.** Collect the forces, constraints, chosen direction, known consequences, links, and any serious alternatives that explain the choice. Do not invent alternatives merely to fill a section.
4. **Choose the smallest shape.** Use the house template. Otherwise read `references/industry-examples.md` and start with Nygard’s five sections; add MADR elements only when they improve the record.
5. **Write one decision.** Describe context without smuggling in the conclusion, state the decision plainly, and record positive, negative, and neutral consequences honestly.
6. **Apply the lifecycle.** Use the local review and status rules. Unless the house process says otherwise, preserve accepted content and use a new linked ADR when the decision changes. Place the artifact wherever the session or repository requires.

## Lifecycle and Writing Rules

- Split unrelated decisions or link them explicitly.
- Explain serious alternatives when they materially clarify the choice or the house template requires them.
- Treat Proposed as a review state only when the house process does.
- Do not rewrite history to make an old decision look correct in hindsight.
- Link implementation, RFC/RFD context, tests, migration notes, and superseding records when those artifacts exist.

## Gotchas

- `adr-tools`, MADR, AWS, and individual projects do not share one universal lifecycle.
- “Accepted ADRs are immutable” is a common practice, not a file-format law. Some teams permit status updates, corrections, or dated follow-ups while preserving the original decision.
- An ADR is not a design document, meeting transcript, or task plan. Deep implementation detail belongs elsewhere.
- “Accepted” does not mean risk-free. Include known downsides and conditions that could invalidate the choice.

## Validation Loop

Before accepting an ADR, check that:

- it contains one clear decision and the correct local status;
- the context names the forces and constraints that made the choice non-obvious;
- alternatives are included when they explain the rationale, not as filler;
- consequences include costs and follow-up work, not only benefits;
- numbering, location, index entries, and links make the record discoverable;
- review, mutation, and supersession follow the house rules.

If the decision is not ready, leave it Proposed or move the discussion into the appropriate RFD or RFC.

## Resources

- Read `references/industry-examples.md` when selecting an ADR shape, lifecycle, or level of detail.
- Read `assets/example-adr.md` for an attributed guide to a real accepted ADR that the source project identifies as canonical.
