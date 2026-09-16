---
name: rfc-authoring
description: >
  Helps author and review Requests for Comments and equivalent prospective design proposals.
  Use when a substantial, cross-team, public-interface, or hard-to-reverse change needs explicit
  review and a defined decision before implementation.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0.1"
---

# RFC authoring

## Overview

Use an RFC to propose a concrete change before commitment. It should make the problem, proposed direction, trade-offs, risks, and requested decision easy to review. Follow the repository or organization’s RFC process and template first; this skill supplies a portable baseline, not a replacement for a house standard.

## When to Use

Use an RFC when the proposal is substantial, affects multiple teams or public interfaces, has competing viable approaches, is expensive to reverse, or needs explicit review and a defined decision before implementation.

## When NOT to Use

- Use an RFD when the problem or solution space is still exploratory.
- Use an ADR when a bounded decision is already made or does not need a formal proposal cycle.
- Use the house artifact when local vocabulary assigns this work to a design doc, proposal, KEP, enhancement issue, or another process.
- Do not create an RFC merely to document implementation details that normal code review can settle.

## Authoring Procedure

1. **Find the house standard.** Search the repository and contribution docs for an RFC template, examples, status vocabulary, owners, and approval rules. Follow them when present.
2. **Confirm the artifact.** State the requested decision, affected parties, reversibility, and why ordinary issue or code review is insufficient. Switch artifacts if the work is still exploratory or already decided.
3. **Gather missing context.** Identify goals, non-goals, constraints, serious alternatives, affected interfaces, rollout risks, decision-makers, and unresolved questions. Ask rather than invent consequential facts.
4. **Choose the smallest useful shape.** Use the house template. Otherwise read `references/industry-examples.md` and select the closest documented shape; do not combine every example into one document.
5. **Draft for the current review stage.** Put the summary and requested decision first. Separate facts from assumptions, explain the proposal at an accessible level before implementation detail, and state what feedback is wanted now.
6. **Run the validation loop.** Fix omissions, then place the artifact wherever the session or house process requires. The skill does not choose a storage location.

## Review Workflow

- Name decision-makers and affected stakeholders. Distinguish required approval from useful input.
- Revise the living draft as feedback changes the proposal. Preserve material objections and their disposition in the document or review record.
- Do not infer acceptance from silence. Apply the local approval rule, such as owner sign-off, steward decision, or a final comment period.
- After approval, link implementation and resulting ADRs. An RFC does not replace later tactical decision records.

## Gotchas

- “RFC” is not one universal lifecycle. IETF, Rust, Mozilla, Go, Apache projects, and other communities use different artifacts, gates, and meanings.
- A design document is not automatically an RFC. The distinguishing feature here is a prospective proposal with an explicit review and decision path.
- Do not hide unresolved disagreement in a polished Decision section. Mark it as unresolved and identify who decides.
- Avoid invented certainty. Label estimates, dependencies, and assumptions so reviewers know what to challenge.

## Validation Loop

Before requesting a decision, check that:

- the proposal answers who is affected, what changes, why now, and what approval means;
- goals and non-goals are testable and serious alternatives are represented fairly;
- relevant compatibility, operational, security, migration, and rollback risks are covered;
- unresolved questions have owners or a clear route to resolution;
- the document follows house naming, numbering, review, and archival rules.

Revise until the requested decision and remaining work are unambiguous.

## Resources

- Read `references/industry-examples.md` when no house template exists or when comparing RFC shapes and governance practices.
- Read `assets/example-rfc.md` for an attributed guide to a real accepted RFC; use its lessons rather than copying its project-specific content.
