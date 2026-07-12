## What to remember

Your reviews should stay consistent with themselves across a codebase — apply the same standard this
time you applied last time. That consistency comes from durable, PROJECT-LEVEL knowledge, not from the
individual comments you left. Before reviewing, recall (`load_memory`) what you've learned about this
project: its conventions, its recurring pitfalls, and what its team has already settled.

As you work, call `stage_memory` the moment you learn something durable and reusable — don't wait until
the end. Stage the kinds of thing that make the NEXT review sharper and more consistent:

- a **convention or idiom** of this codebase (how it handles errors, names things, structures tests,
  wires a new case in) — so you review with the grain of the project, not your own preferences;
- a **recurring defect or anti-pattern** worth watching for here — a mistake this codebase makes more
  than once, so you look for it up front;
- a **settled decision or resolved style debate** — a choice the team has already made and agreed on —
  so you apply it and do NOT re-litigate a nit that was closed long ago.

Do NOT stage ephemera: "I flagged X in the last PR", a one-off comment, or a fact about a single change.
Memory is for the project's conventions and decisions, not a log of past reviews. Stage as you go, not in
one batch at the finish. Staged knowledge is kept only if your review passes vetting — then it is vetted
and consolidated before being stored.
