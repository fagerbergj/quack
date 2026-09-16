# Real RFC example: The Rust RFC process

- **Project:** Rust
- **RFC:** 0002
- **Start date:** 2014-03-11
- **Status:** Accepted; established Rust’s RFC process
- **Canonical document:** [rust-lang RFC 0002](https://rust-lang.github.io/rfcs/0002-rfc-process.html)
- **Source repository:** [rust-lang/rfcs](https://github.com/rust-lang/rfcs/blob/master/text/0002-rfc-process.md)

This is a curated excerpt and reading guide, not a vendored copy. The source repository offers Rust RFC content under its repository licenses.

## Why this example is useful

RFC 0002 is both a real accepted proposal and the foundation of one of the best-known open-source RFC processes. It states the problem, defines scope, specifies an operational process, distinguishes changes that do and do not require the process, compares alternatives, and leaves genuine unresolved questions.

## Published structure

- Summary
- Motivation
- Detailed design
  - When the process applies
  - What the process is
- Alternatives
- Unresolved questions

## Excerpt

> The “RFC” (request for comments) process is intended to provide a consistent and controlled path for new features to enter the language and standard libraries, so that all stakeholders can be confident about the direction the language is evolving in.

The RFC then explains why Rust’s earlier informal approach no longer fit, enumerates substantial changes that require an RFC, documents the proposal lifecycle, and compares both a lighter and a stricter alternative.

## What to learn from it

- State the intended outcome in the summary, not only the topic.
- Define when the proposal applies and when it does not.
- Make governance executable by naming concrete steps and decision authority.
- Compare the proposal with both lighter and heavier alternatives.
- Leave unresolved questions visible instead of manufacturing completeness.

Read the [complete RFC](https://rust-lang.github.io/rfcs/0002-rfc-process.html) before using it as a model.
