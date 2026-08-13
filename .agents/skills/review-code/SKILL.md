---
name: review-code
description: >
  The repeatable process for reviewing a code change: understand the change's
  intent before critiquing, read the diff AND the surrounding code, verify the
  change's claims and tests rather than trusting them, categorize each finding
  by severity, and structure the written review. Load whenever the task is to
  review a pull request, diff, branch, or proposed code change - before you
  start reading the diff.
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
---

# Review code: understand → verify → categorize → structure

A good review improves the codebase's health - it is not a symptom scan of the diff. Reviewing lines in isolation misses the design flaw, trusts a test that never fails, and buries the one real bug under style nits. This process reviews the change the way it will actually be lived with.

Do not write a single finding until step 2: you cannot judge a change you don't yet understand.

## The workflow

### 1. Understand the intent and context first

Before looking critically at any line, learn what the change is *for*.

- Read the PR description / linked issue / commit messages. State in one sentence what problem this change claims to solve and how.
- Get the change in front of you: `git_diff` (or `git_log` for the branch's commits). Take a broad view first - which files were added, deleted, moved - to grasp the intent before the details.
- If you can't tell what the change is meant to do, that's your first finding (a `question:`), not a licence to guess.
- **Understand the PR's current state before forming your review.** Read what has already been said: on a GitHub PR, `github_list_pr_comments` returns the existing inline comments, the conversation, and prior submitted reviews. And recall this repo's settled decisions and conventions with `load_memory`. Know what ground has already been covered so your review adds to it.
- **Do not re-raise anything already settled or already discussed.** A resolved thread, a maintainer-accepted explanation, a prior ruling ("we do X here because Y") is closed - applying it and moving on is the point of remembering it. If you genuinely disagree with a settled decision, note the concern *once* and move on; don't reopen the same debate every review.

### 2. Verify the claims - don't trust them

"This fixes X", "this improves performance", "the tests cover it" are claims. Check each against the actual diff before you accept it.

- Does the code actually do what the description says? Trace the changed path and confirm the stated behavior is really there.
- Is the change in the right part of the system, or bolted somewhere convenient? A four-line change can be correct in isolation yet wrong for where it lives.
- Never assert behavior about code you didn't open. `read_file` the file before claiming what it does - a finding about code you never read is fabrication.

### 3. Read the diff AND the surrounding code

Use `git_diff` to see *what* changed - but never review from the diff hunks alone. A hunk shows a handful of lines with a sliver of context and hides everything a real judgment needs: the caller, the invariant two methods up, the error the callee can return, the lock the function is supposed to hold. **`read_file` the full files the change touches**, not just the changed lines, and grep for other call sites a signature change affects. Judge the change in the code it actually lives in - a four-line diff can be correct in isolation and wrong for the function it sits inside.

### 4. Evaluate by priority - spend attention where it matters

Weight your scrutiny by impact on code health, top-down:

1. **Design & context** - Does this belong here, now? Watch for over-engineering: abstraction or flexibility for a hypothetical future need, not a present one. If you find a fundamental design flaw, say so *immediately* - don't finish the whole diff first; catching it early spares the author wasted work.
2. **Correctness** - Think like a user. Edge cases (empty input, network failure), concurrency (races, deadlocks), unhandled error paths.
3. **Security** - Injection, missing authorization, secret handling, unsafe input. A real security flaw is blocking.
4. **Tests** - see step 5; tests get their own pass.
5. **Complexity & readability** - If you must trace every variable to understand a function, it's too complex; ask the author to simplify.
6. **Naming, docs, style (lowest)** - Usually nits. On style, the project's linter/style guide is the authority; if there's none, accept the author's preference - don't invent one.

### 5. Check the tests as rigorously as the code

Tests are not self-validating - a human must confirm they check for meaningful failure.

- Walk the new tests alongside the new logic: **would they actually fail if the code were broken?** A test that passes no matter what is worse than none.
- Do they cover the new branches, the edge cases, and the failure modes - or only the happy path?
- New behavior with no test is a `blocking:` gap, not a nit.

### 6. Categorize every finding by severity

Label each finding so the author isn't overwhelmed (Conventional Comments). The line is whether you can point at something objective, not how strongly you feel: a finding supported only by your preference is a `nit:`, however sure you are. `references/severity.md` carries the anchors a `blocking:` finding must name (defect, security, design, scope, tests), the hard rules - style guide is the authority, out-of-scope belongs in its own issue, forward progress beats polish - and worked calls in both directions. Read it whenever a label isn't obvious.

- **`blocking:`** - a definite bug, security hole, or major design flaw; must be fixed before merge.
- **`suggestion:`** - an improvement; state *why* it's better. Non-blocking unless you give a compelling reason.
- **`nit:`** - trivial, preference-based; NEVER blocks.
- **`question:`** - you suspect a problem but aren't sure; asking resolves it faster than demanding a change.
- **`praise:`** - something genuinely well done. Keep it in the summary, at most two sentences, and NEVER as an inline comment - inline praise litters the diff the author has to read past to find what actually needs doing.

Add a decoration when it sharpens the point: `blocking (security):`, `suggestion (performance):`. Explain the *why* on every non-trivial finding - the principle or risk - so it's actionable and the author learns.

### 7. Structure the written review and state a verdict

Don't scatter line-by-line comments. Deliver:

1. **Summary** - a high-level, constructive takeaway that sets the tone and states your verdict (e.g. "Solid approach overall; two correctness issues to resolve before merge").
2. **Blocking issues** first, then **suggestions**, then **nits** - grouped by severity so the author fixes what matters first. Praise stays in the summary, not in this list.
3. **Verdict** - *request changes* if any blocking issue stands; *approve* once they're resolved. Favor approving a change that clearly improves code health over holding it hostage to nits - and if you approve with non-blocking comments, say explicitly that they're non-blocking.

Cite findings by `path:line` so the author can jump straight to them. Write the summary and each finding following the shared style ruleset at `~/.agents/AGENTS.md` - concrete, no ceremony, no canned "great work!" filler.

On a GitHub PR, deliver this as one native review: record each ACTIONABLE finding (never praise) as an inline comment (`github_add_review_comment`, path+line) the moment you spot it - the draft is your durable memory against context compaction - then `github_submit_review` once with the summary body and the event matching your verdict. Reply in-thread (`github_reply_to_review_comment`) when you're responding to an existing comment rather than opening a new thread.

**Close loops with a reaction, not a comment.** To acknowledge a point, signal agreement, or say "seen / handled", add an emoji reaction (`github_react_to_comment` - 👍 / 👀 / 🚀) instead of writing another comment. It closes the loop with near-zero words and low reader load. Reserve comment text for substantive findings.

## Why this order

You can't judge a change you don't grasp, so understanding comes first. Verification comes before critique for the same reason: a review that trusts the author's claims is a rubber stamp, not a review. And severity outranks structure because the author's time is finite - block on what's genuinely wrong, and let the rest be suggestions.

## When this is lighter

A tiny, self-evidently correct change (a typo fix, a version bump, a one-line guard) doesn't need the full pass - verify it does what it says, check nothing else broke, and approve. The full loop is for any change whose correctness, design, or test coverage isn't immediately obvious - which is most of them.
