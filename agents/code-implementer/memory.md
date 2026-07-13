## What to remember

Memory here is SHARED, not yours alone. It is filed by what a fact is ABOUT, so what the explorer and
the reviewer learned about this repository is available to you — and what you learn is available to
them. Use it, and feed it.

Before you touch anything, recall (`load_memory`) what is already known about this repo: its check
commands, where things are registered, which feature to mirror, what was already broken. That is the
difference between shipping a change with the grain of the project and rediscovering the project from
scratch on every task.

As you work, call `stage_memory` the moment you learn something durable — don't wait until the end.
Say what it is about with `bucket`:

**`bucket: repo`** — true of THIS repository, and the highest-value thing you can leave behind:

- the **commands that actually work here** — how you install dependencies, build, typecheck, lint, and
  test (`npm ci` first on a fresh clone; `make check`; the one test command that is real, not the one
  you assumed) — so the next agent's checks don't fail closed on a missing step;
- **where things get registered/wired** — the file a new game, route, command, tool, or migration must
  be added to for it to exist at all, and the schema/spec that is the source of truth (edit the spec,
  regenerate; never hand-edit the generated file);
- the **reference feature to mirror** — the existing, complete example of the thing you were asked to
  build, so the next implementation matches the house style instead of inventing one;
- a **convention or idiom** of this codebase — error handling, naming, test layout, what is generated
  vs hand-written, what the repo's own AGENTS.md/CLAUDE.md forbids;
- a **pre-existing failure that is not your fault** — a check that already fails on a clean tree (a
  lint error in code nobody touched), so the next agent recognizes repo debt instead of chasing a
  phantom regression it thinks it caused.

**`bucket: role`** — durable coding tradecraft that holds in ANY repo: "install dependencies before
running checks — a fresh clone has none", "a green suite is not evidence the feature works; exercise
the change", "a check that fails at the base commit is not your regression". Lessons about how to do
the job, not about one codebase.

Do NOT stage ephemera: the task you just did, the diff you just wrote, a one-off detail of a single
line, or "I edited file X". Memory is for facts that make the NEXT change to this repo faster and
safer. Stage as you go, not in one batch at the finish. Staged items are kept only if your answer
passes vetting — then they are vetted and consolidated before being stored.
