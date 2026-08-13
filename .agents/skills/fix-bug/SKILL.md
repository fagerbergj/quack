---
name: fix-bug
description: >
  The disciplined way to fix a bug: build a root-cause theory by reading the
  code, prove it with a test that fails for the right reason, then implement
  until that test goes green and the wider suite stays green. Load whenever the
  task is to fix a bug, defect, regression, crash, wrong output, or "it should
  do X but does Y" in existing code - before you start editing.
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
---

# Fix a bug: theory → failing test → green

A bug fix is not "change code until the symptom goes away." That produces fixes that paper over the wrong cause, break something else, or silently stop working. The discipline below makes the fix *provably* address the real cause.

Do not edit production code until step 2 gives you a test that fails.

## The workflow

### 1. Build a root-cause theory by exploring the code

Read the actual code paths involved - `read_file`, `grep`, `git_log`/`git_diff` to see what recently changed. Trace how the input reaches the wrong output. State the theory in one sentence: *"X happens because Y, so Z is wrong."*

- A theory names a **specific mechanism**, not a vague area. "The confirm scan never runs on revise rounds" is a theory; "something in the gate is off" is not.
- If you cannot yet see the mechanism, **instrument to confirm it** - add temporary logging / a debug print / a probe, reproduce, and read what actually happens. Real bugs are often not where they look. Confirm the mechanism *before* committing to a fix. (Remove the instrumentation before you finish, or fold it into the test.)

### 2. Confirm the theory with a failing test

Write a test that reproduces the bug and **fails for the right reason** - it fails *because of the defect you theorized*, not because of a typo or an unrelated gap. Run it, read the failure, and confirm the failure message matches your theory. This is the moment the theory becomes fact.

- If the test passes unexpectedly, your theory is wrong - go back to step 1.
- If it fails for a *different* reason than predicted, your theory is incomplete - refine it until the failure is exactly what you expected.
- Prefer the smallest test that captures the defect at the level it lives (unit if the bug is in a function; integration/e2e if it only appears across a boundary - the A2A/session bugs only reproduce end-to-end).

### 3. Implement until the failing test passes

Now - and only now - change the code. Make the failing test go green with the smallest change that addresses the *mechanism*, not the symptom.

- Then run the **broader suite** (`go test ./...`, the frontend tests) to prove you fixed the bug without breaking anything else. A green new test plus a red old one is not a fix.
- Keep the reproducing test - it's now a regression guard. A future change that reintroduces the bug fails loudly.

## Why this order

The failing test is the contract. Written first, it proves the bug exists, proves you understand it, and proves - the moment it goes green - that you fixed *that* bug and not a lookalike. Written after, it only proves the code does what the code does.

## When this is overkill

A one-line typo with an obvious, self-evident cause (a wrong constant, a flipped boolean whose effect is plain) doesn't need a ceremony - fix it and add a check if the logic is non-trivial. The full loop is for any bug where the cause isn't immediately certain, which is most of them.
