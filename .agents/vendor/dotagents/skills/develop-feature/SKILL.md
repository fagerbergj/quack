---
name: develop-feature
description: >
  The disciplined way to build a new feature: understand the real problem, reuse
  or extend before you add, prefer a proven library over hand-rolled code, work
  the edge cases until the design holds, gate on functional tests, then
  implement to green. Load whenever the task is to add a feature, capability,
  command, endpoint, or non-trivial new behavior to a codebase - before you
  start designing or writing code.
---

# Develop a feature: understand → reuse → tests-as-gate → green

Most bad features come from building the wrong thing, or building from scratch what already existed. This workflow front-loads understanding and reuse so the code you finally write is the minimum that's actually needed.

The order matters: do not design before step 1, and do not write feature code before step 5 gives you tests that fail.

## The workflow

### 1. Quiz until you truly understand the problem

Do not start from the literal request - start from the problem behind it. Ask until you can state the goal, the constraints, and what "done" looks like in your own words.

- Use `ask_user` to clarify - but **batch your questions and ask early**. Ask once with the handful of things you genuinely can't resolve, not one at a time across many pauses. (Asking pauses the whole node; thrashing is expensive.)
- Only ask what you can't answer yourself. Where the answer is conventional, pick the sensible default, state it, and move on - reserve questions for decisions that are genuinely the user's to make.
- Restate the problem back before designing. If you can't, you don't understand it yet.

### 2. Reuse or extend before you add

Before designing anything new, find what's already there:

- Is there an **existing feature that can be widened** to cover this use case?
- Are there **natural extension points** already in the code - an interface, a hook, a registry, a config seam - meant to be extended? Read the surrounding code to find them (`grep` for the interface, read how existing cases plug in).

Extending a proven seam beats a parallel new mechanism almost every time. A new subsystem that duplicates an existing one is a smell.

### 3. Prefer a proven library over hand-rolled code

Walk the ladder, stop at the first rung that holds: **does this need to exist at all → stdlib → native platform/framework feature → an already-installed dependency → a well-maintained library → only then, the minimum code you write yourself.**

- If a standard or OSS library gets you most of the way, use it. Don't reimplement crypto, parsing, retries, date math, HTTP plumbing.
- If a library gets you *most* of the way but is missing a piece, **ask the user whether the missing bit can be negotiated away** - a scoped-down requirement that keeps you on the maintainable library path is usually the better trade than a bespoke implementation you now own forever. Surface that trade explicitly; let them choose.

### 4. Work the edge and corner cases - before coding

Enumerate the edge cases (empty input, huge input, concurrency, failure partway, auth boundaries, the second call, resume/retry) and answer, for each: **do you actually know how you'd handle it?**

- If an edge case has no clean answer, you're not ready to code - resolve it first.
- If an edge case **breaks your design**, revisit the design *now*, not after you've written to it. An edge case that changes a design principle is telling you the design is wrong; listen before there's code to throw away.

### 5. Write functional tests - this is your gate and barometer

Write the tests that define "the feature works" before the implementation. They are the contract and the finish line: while they're red you're not done, when they're green you are.

- Cover the real behavior and the edge cases from step 4 - a test per corner case you decided how to handle.
- Test at the level the feature lives (functional/integration for a cross-boundary feature; unit for a contained one).

### 6. Implement until the tests pass

Write the minimum code that turns the tests green. No speculative abstraction, no scaffolding "for later" - the tests define the scope; build to them and stop. Then run the broader suite to confirm nothing else broke.

## Why this order

Understanding and reuse come first because the cheapest feature is the one you didn't have to build - an extension of what exists, or a library call. Tests come before code because they're what turns "I think it works" into "it provably does what we agreed it should."

## When NOT to use

A truly trivial addition with an obvious shape and no design questions (a new enum value, a passthrough field) doesn't need the full loop. This is for any feature where the *design* isn't self-evident - where getting it wrong is cheaper to catch in step 1–4 than after it ships.
