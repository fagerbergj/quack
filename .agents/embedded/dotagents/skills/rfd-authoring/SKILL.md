---
name: rfd-authoring
description: >
  Helps author and review exploratory Requests for Discussion and equivalent early-stage documents.
  Use when a problem, opportunity, or technical question needs shared framing before choosing a
  concrete solution or escalating to a formal proposal or decision record.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0.1"
---

# RFD authoring

## Overview

When no house definition exists, this skill treats an RFD as an exploratory, pre-proposal document. Its job is to make a problem discussable, expose missing information, and determine the next decision. RFD is not a standardized artifact name; some organizations use it for a concrete proposal or scheduled discussion instead.

## When to Use

Use this default when the question matters but the solution, scope, evidence, or decision-maker is unclear and a shared written frame would make discussion productive.

## When NOT to Use

Check this list before drafting, not after. Where one of these fits, the answer is the other artifact - not an RFD carrying a disclaimer.

- Follow the house definition when the organization already defines RFD differently.
- Use an RFC when a concrete proposal is ready for structured review and decision.
- Use an ADR when a bounded decision has been made and needs a durable record.
- Use an issue, meeting agenda, or short message when the discussion is local, reversible, and does not need a lasting artifact.

## Authoring Procedure

1. **Find the house definition.** Search the repository and governance docs for RFD examples, templates, labels, meeting rules, voting rules, and dispositions. Local meaning overrides this skill’s default.
2. **Confirm the discussion need, and stop if there is none.** State the question, affected people, uncertainty, and why an issue comment or ordinary meeting is insufficient. If nothing is actually undecided - the choice is already made, the requirement is external and fixed, the mechanism already exists, or the change is small and reversible - say so plainly and write what the situation does need instead: a durable record, a plan of the work, a request for a named sign-off, or an account of how the thing already behaves. Exiting here is a correct outcome of this skill, not a failure to apply it.
3. **Gather known context.** Separate facts, assumptions, constraints, candidate directions, missing evidence, and the people whose input matters. Do not invent a preferred solution to make the document look complete.
4. **Choose a shape.** Use the house template. Otherwise read `references/industry-examples.md` and use the exploratory default there; the Nebari and LSST examples demonstrate that RFD processes can differ sharply.
5. **Draft for discussion.** Put the central question and desired outcome first. Ask specific questions whose answers could change the next step. Label any provisional direction as tentative.
6. **Close the loop.** Record the disposition: stop, gather evidence, open an RFC, make and record a small decision, or schedule further discussion. Place the artifact wherever the session or house process requires.

## Discussion Workflow

- Confirm the audience and where comments or synchronous discussion happen.
- Invite affected users and operators as well as likely implementers.
- Capture objections as constraints or questions; not every comment becomes a requirement.
- Do not infer agreement from silence. Apply any local response, voting, or approval rule.
- Move a concrete proposal into the house RFC or proposal workflow instead of letting an exploratory RFD grow without bounds.

## Gotchas

- Nebari uses RFDs for proposals that can proceed to a vote; LSST uses them to organize in-depth technical discussions. Neither establishes a universal meaning.
- An exploratory RFD should expose uncertainty, not manufacture consensus - and equally, not manufacture uncertainty. An Open Questions section listing things nobody is actually asking exists to justify the document, and is worse than not writing one.
- “Discussion” is not a useful status unless the document says what input is wanted and what ends the discussion.
- Do not record a tentative direction as an accepted architectural decision.

## Validation Loop

Before sharing, check that:

- the central question and desired outcome are understandable without hidden context;
- facts, assumptions, constraints, candidate directions, and unknowns are distinguishable;
- each discussion question could affect the disposition;
- the owner, audience, discussion channel, and closing rule are named;
- the document follows house naming, status, review, voting, and archival conventions.

After discussion, record the disposition and link the next artifact. If nothing changed, say why the discussion is closing rather than implying approval.

## Resources

- Read `references/industry-examples.md` when the local RFD format is unclear or when choosing between an exploratory discussion, proposal-and-vote, or scheduled-review process.
- Read `assets/example-rfd.md` for an attributed guide to a real accepted RFD; use its lessons rather than copying its project-specific content.
