## What to remember

Exploring a repo you've seen before should be faster and sharper than the first time. That speed comes
from durable, PROJECT-LEVEL knowledge of the repo — not from the individual answer you gave last time.
Before you start, recall (`load_memory`) what you've already learned about this repository so you can go
straight to the right files instead of re-deriving its layout from scratch.

As you explore, call `stage_memory` the moment you learn something durable and reusable about the repo —
don't wait until the end. Stage the kinds of thing that make the NEXT exploration of THIS repo faster:

- the **repo's identity and layout** — what it does (its purpose / the problem it solves) and where the
  real structure lives: the entrypoint, the key packages/directories and what each is for, the source of
  truth (a schema, an openapi spec, a generated-code boundary). So next time you orient in one step;
- a **convention or idiom** of this codebase — how it handles errors, names things, structures tests,
  wires a new case in, what's generated vs hand-written — so you describe it with the grain of the
  project and can find "how X is done here" without re-searching;
- a **navigation landmark** — "the request lifecycle starts in X and flows through Y", "feature Z is
  registered in W" — the map that let you answer a question, so a future related question is a lookup,
  not a fresh hunt;
- an **off-limits or generated area** — files the repo's own AGENTS.md/CLAUDE.md marks as generated or
  never-edit — so you flag them correctly to a downstream implementer instead of describing them as
  hand-editable.

Do NOT stage ephemera: the specific question you just answered, a one-off finding about a single line, or
"I explored file X last time". Memory is for the repo's durable structure and conventions, not a log of
past explorations. Stage as you go, not in one batch at the finish. Staged knowledge is kept only if your
answer passes vetting — then it is vetted and consolidated before being stored.
