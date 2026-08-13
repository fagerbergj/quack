---
name: pr-authoring
description: >
  How to write a pull request - a title and a description that tell the reviewer
  what changed, why, and how to verify it, scaled to the size of the change. Use
  when composing a PR for delivery.
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
---

# PR Authoring

A pull request exists to **communicate with the reviewer**, and the author controls review latency: a clear "what + why + how to verify" gets merged in minutes; a bare diff with "various fixes" sinks to the bottom of the queue. Write the description for someone walking in blind - not from your own memory of the work.

## Title

`type(scope): subject` - imperative, ≤50 chars, no period (`feat(auth): add Google OAuth2 sign-in`). It becomes the squash commit on the default branch, so it's permanent history, not a label. Reference the issue: end with `(closes #<n>)` or put `Closes #<n>` in the body. If you can't title the change in one line, it's too big - it should be more, smaller PRs.

## Template - repo's first, ours as fallback

Before writing the body, pick the template:

1. **Read the target repo's `.github/pull_request_template.md`** (`read_file`). If it exists, fill *its* sections - respect the repo's own conventions over ours.
2. **Otherwise** use this skill's `assets/default-pr-template.md` as the skeleton.

Either way, delete sections that don't apply - a two-line fix keeps only Why + Verification.

## Description - answer what the diff can't

Every description answers **what** changed, **why**, and **how to verify** it. Write it concrete over generic, no ceremony. Add the rest as the change warrants:

- **Why** - the problem and context (one paragraph). Without it the reviewer guesses.
- **What** - the change at a glance. For several commits, a short list in reading order.
- **How** - the approach and the one or two decisions touching architecture, existing behaviour, or downstream systems, with tradeoffs. Skip the obvious.
- **Verification** - how you confirmed it works: tests added, commands run + result, steps to reproduce, screenshots for UI. Pre-empts the "did you test it?" round trip.
- **Callouts / feedback wanted** - point the reviewer at the specific lines or decisions that most need their eyes ("the 5s timeout at handler.go:142 - right value?"). Telling the reviewer *what feedback you want* is the single most predictive thing for a good review.
- **Out of scope** - for a feature, name the deliberate follow-ups; pre-empts "shouldn't this be fixed too?".

## Scale to the change

| | Small fix | Feature |
|---|---|---|
| Description | two sentences: the bug and the fix | Why / What / How / Verification, tradeoffs |
| Verification | one regression test + CI green | tests + steps/screenshots |
| Out of scope | omit | list follow-ups |
| Breaking change | n/a | flag prominently with migration steps |

## Rules

- **One PR = one idea.** Never mix a refactor with a behaviour change; no "while I'm at it" extras - they bury the signal.
- **Link, don't make them search** - the issue, related PRs, and (for a designed change) the design doc/spec. Reference its decision; don't re-argue it in the PR.

## Anti-patterns

`Refactored service layer` / `Updated config handling` (written from memory, says nothing) · a wall of *what* with no *why* · describing the diff instead of the intent · no callout, so the reviewer has to hunt for the risky part themselves.
