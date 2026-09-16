---
name: comment-authoring
description: |
  Authors and audits code comments that explain intent, constraints, invariants, workarounds, security, performance, or public API contracts without narrating the code. Use when writing comments, TODOs, docstrings, godoc, Javadoc, JSDoc, or TSDoc; reviewing comment quality; or updating comments beside changed code.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "2.0.1"
---

# Comment Authoring

## Overview

Comments say what code cannot. First make the code clear through names and structure; then comment only the intent, constraint, or contract a competent maintainer could otherwise misunderstand. Local comments stay short. Public API documentation follows the language's tooling convention and may be longer when the contract requires it.

## When to Use

- Writing or revising an inline, block, file-level, or public API comment.
- Adding a TODO, workaround note, security rationale, or performance constraint.
- Reviewing a diff for stale, redundant, or missing comments.
- Changing code next to an existing comment.

## When NOT to Use

- Put change history, incident narratives, and review context in the commit or PR.
- Put durable architectural decisions in an ADR.
- Rename, extract, or simplify code instead of commenting around unclear structure.
- Do not add comments merely to increase documentation coverage.

## Step-by-Step Procedure

- [ ] **Read the code in context.** Check callers, tests, adjacent comments, and the surrounding file's established style.
- [ ] **Try code first.** Rename or restructure when that makes the intent obvious without prose.
- [ ] **Apply the decision test.** Could a competent reader be misled about intent, constraints, or the contract using only the code and good names? If no, do not comment.
- [ ] **Choose the comment's job.** State one of: rationale, invariant, workaround, edge case, performance or security constraint, public contract, or actionable future work.
- [ ] **Write at the narrowest scope.** Put the comment immediately above the code it governs; use file-level context only when one fact explains the whole file.
- [ ] **Verify it against the code.** Remove or update any adjacent comment made stale by the change.

## What Earns a Comment

- **Intent or tradeoff:** why the obvious implementation is wrong here.
- **Invariant or contract:** an assumption or guarantee not enforced by types or tests.
- **Workaround:** the external bug or limitation forcing this shape, with an issue, spec, or vendor link when it helps future removal.
- **Subtle edge case:** the specific case a reasonable editor could break.
- **Performance or security:** the measured cost or threat that makes surprising code necessary.
- **Public API:** behavior, errors, ownership, side effects, and limits callers must know.
- **TODO:** the action, reason for deferral, and accountable ticket or owner when one exists.

## Delete These

- Line-by-line narration or English translations of names and statements.
- Comments that compensate for a bad name or needlessly complex structure.
- Stale comments, commented-out code, separator banners, praise, apologies, and venting.
- Vague warnings such as `magic`, `handles errors`, or `do not touch` without the concrete constraint.
- History that does not help a maintainer preserve or remove the current behavior.

## Local Comments

Keep inline and local block comments to one or two lines. Use problem-domain language and explain why, not what.

```go
// Retry after refresh: the first token may expire between lookup and use.
return client.Call(refresh(ctx))
```

A bug or ticket link belongs in source only when it identifies a live workaround, specification, or removal condition. Dates, incident timelines, and who found the bug belong in the commit or PR.

## Public API Documentation

Document contracts, not signatures. Follow the repository's language and tooling convention; public does not mean every declaration needs prose when the project deliberately relies on generated or self-describing APIs.

For Go, attach `// Name ...` directly above the declaration. For Python, use a PEP 257 docstring. For Rust use rustdoc, and for Java, C++, JavaScript, or TypeScript use the project's Javadoc, Doxygen, JSDoc, or TSDoc convention.

## TODOs

Use `TODO(owner-or-ticket): action and reason`. A TODO without a next action or removal condition is not a plan. Never keep commented-out code; version control already stores it.

```java
// TODO(SEC-231): use constant-time comparison after upgrading crypto to 1.4.
```

## Gotchas

- A wrong comment is worse than no comment. Treat comment updates as part of the behavior change.
- The one-or-two-line ceiling applies to local comments, not API contracts that genuinely need more detail.
- An issue link without a stated constraint makes readers leave the file to learn why the code exists.
- A comment can explain why a deliberate inefficiency remains; it cannot substitute for evidence that the tradeoff matters.

## Validation Loop

1. Read the code once without the comment. Delete the comment if names and structure already provide the same information.
2. Check that every claim is true for the current implementation and its callers.
3. Confirm the comment states one job, sits at the right scope, and follows language tooling rules.
4. Search the diff for commented-out code, stale references, vague TODOs, and narration.
5. Run `git diff --check`.

## Resources

Read `references/sources.md` when documenting a public API in an unfamiliar language, choosing a TODO convention, or citing the research behind these rules.
