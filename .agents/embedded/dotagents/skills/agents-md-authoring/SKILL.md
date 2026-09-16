---
name: agents-md-authoring
description: |
  Authors, rewrites, and audits AGENTS.md instruction files using a discoverability-first rubric. Use when creating AGENTS.md, CLAUDE.md, or repository agent instructions; pruning stale or redundant guidance; adding nested instructions; or reviewing whether a rule earns permanent context.
license: MIT
metadata:
  author: fagerbergj
  version: "1.0"
---

# AGENTS.md Authoring

## Overview

Write the smallest instruction file that changes agent behavior. Keep facts an agent cannot reliably discover from the repository, and delete the rest. This skill covers instruction content and placement; use repository tooling or CI for rules that must be enforced.

## When to Use

Use this skill when asked to:

- create, rewrite, shorten, or review an `AGENTS.md` or `CLAUDE.md` file;
- add instructions for a repository or subtree;
- move a convention into agent context;
- audit agent instructions for noise, conflicts, or staleness.

## When NOT to Use

- Do not use it for human onboarding or contributor documentation.
- Do not edit generated, cached, vendored, dependency, or worktree copies unless they are tracked source files owned by the repository.
- Do not rely on prose for mandatory policy. Put enforceable rules in tests, linters, hooks, or CI.

## Step-by-Step Procedure

- [ ] **Set scope.** Find tracked instruction files and their repository roots. Deduplicate worktrees and remotes before editing.
- [ ] **Read evidence.** Read the current instructions, README, build manifests, task runners, CI, and relevant docs. Treat existing prose as a claim, not proof.
- [ ] **Audit every line.** Keep a line only when all three answers are yes:
  1. Is it hard to discover by reading the repository?
  2. Does it change an agent's observable decision or action?
  3. Is it current and supported by an authoritative file or a verified command?
- [ ] **Keep high-value guidance.** Prefer exact commands with non-obvious prerequisites, tool choices that invert common defaults, cross-file invariants, safety boundaries, hidden process requirements, recurring failure modes, and pointers to task-specific docs.
- [ ] **Delete noise.** Remove directory tours, dependency lists, framework defaults, README copies, file-by-file summaries, API dumps, history, code snippets, vague quality advice, and formatting rules already enforced by tools.
- [ ] **Place narrowly.** Put global rules at the root. Use nested files only for subtree-specific constraints; they add or narrow guidance and never repeat the root.
- [ ] **Draft imperatively.** Write commands, constraints, and pointers. Prefer `Use just test; do not run cargo test directly` over explanation.
- [ ] **Verify.** Run each retained command when safe, check every pointer, compare against CI, and inspect the final diff for unsupported claims.

## Length and Structure

- Prefer fewer than 60 lines for a root file; treat 150 lines as a review trigger, not a target.
- Omit overview and structure sections when the repository already makes them obvious.
- Use headings only when they improve retrieval. A short flat list is better than empty ceremony.
- Point to authoritative docs instead of copying them.

## Gotchas

- Instruction files are context, not enforcement. A critical invariant needs a mechanical check.
- A true fact about one legacy corner can bias work elsewhere. Scope it in a nested file or omit it.
- Exact commands still need evidence. Package scripts and old README examples can both be stale.
- Never replace a useful non-obvious constraint merely to hit a line budget.
- When root and nested instructions conflict, tool-specific precedence varies. Avoid conflict instead of depending on override behavior.

## Validation Loop

Repeat until clean:

1. Run `git diff --check`.
2. Confirm each retained rule has repository evidence or a successful command result.
3. Ask of every line: could the agent discover this quickly from code? Delete it if yes.
4. Check that nested files do not repeat parent guidance.
5. Confirm the diff contains no generated or third-party copies.

## Resources

No bundled resources. Use the target repository's authoritative files as evidence.
