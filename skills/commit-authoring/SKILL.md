---
name: commit-authoring
description: >
  How to write one atomic commit for a code change - Conventional Commits format,
  imperative subject, why-not-how body. Use whenever you commit code (git_commit):
  one node's work is exactly one reviewable, testable commit.
---

# Commit Authoring

One implementer node = **one atomic commit**. Write the commit so it reads as a single, reviewable, testable unit - and write its message so a reviewer never has to reverse-engineer intent from the diff.

## What "atomic" means (non-negotiable)

- **One concern.** A subject that needs "and" is two commits. Never mix a refactor with a behaviour change - the reviewer can't tell an intentional change from an accident of the refactor.
- **Builds and tests pass at THIS commit.** Rolling back to it leaves a working tree (bisectable, revert-safe). Your node's `checks` enforce this.
- **Under ~400 lines of diff.** Past that, review quality collapses. If the change is bigger, it's more than one commit - it should have been more than one node; flag it rather than growing the commit.

## Message format (Conventional Commits + the seven rules)

```
<type>(<scope>): <subject>      ← ≤50 chars, imperative, Capitalized, no period

<body>                          ← wrap 72; explain WHY, not how (the diff shows how)

<footers>                       ← Refs #123 · BREAKING CHANGE: <what + migration>
```

- **Subject** completes: "If applied, this commit will `<subject>`." Imperative ("Add", not "Added"/"Adds"). No trailing period.
- **Body** is the *why* and the non-obvious *what* - motivation, constraints, the incident. Omit it only for a truly trivial change. Write it plainly - concrete over generic, no ceremony.
- **Footer**: `Refs #<issue>` (the system adds `Closes #N` on the PR, not here); `BREAKING CHANGE:` for any incompatible change, with the migration.

| type | use |
|---|---|
| `feat` | new behaviour |
| `fix` | bug fix |
| `refactor` | behaviour-preserving restructure |
| `perf` | faster, same behaviour |
| `test` | tests only |
| `docs` `chore` `ci` `build` `style` | supporting changes |

One `type` per commit - if two apply, it's two commits.

## Anti-patterns

| Don't | Instead |
|---|---|
| `update` / `wip` / `fix` (bare) | say what and why: `fix(auth): handle null JWT on logout` |
| `Update webhook.go and app.go` (file list) | describe the change, not the files touched |
| Describe the diff (`Remove console.logs`) | give intent: `chore: drop debug logging before release` |
| Ticket number as the whole subject (`ABC-123`) | human subject; put the ref in the footer |
| "and" in the subject | split into two commits |
| Past/inconsistent tense | imperative, always |

## Check before committing

Subject ≤50 and imperative · body says *why* · one concern · one `type` · builds+tests green at this commit · no "and".
