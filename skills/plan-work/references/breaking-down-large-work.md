# Breaking Down Large Work

Load this when a single coding request is **large or multi-phase** and the split into nodes isn't obvious — a whole app or game, a feature touching several layers, several distinct mechanics/screens, or a migration across many files. It gives you a decision procedure for turning one big request into a correct DAG of `code-implementer` (and, when live facts are genuinely needed, `web-researcher`) nodes.

The core shift: decompose by **shippable capability**, not by technical layer. A node should be a vertical slice a user could see working, not a horizontal fragment (the "model" of everything, then the "API" of everything). See §4.

---

## 1. Find the seams before you split

Cut where coupling is naturally low and cohesion high. Look for seams in this order of preference:

- **Feature / user-action seams (preferred).** Each distinct thing the user can *do* — *create comment*, *move character*, *process payment* — is one complete, testable capability, and a natural node boundary. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design)
- **Domain / bounded-context seams.** Isolate by business capability; each context owns its own data model and state, so it makes a clean independent node. [Stacked Pull Requests](https://www.michaelagreiler.com/stacked-pull-requests/)
- **Data seams.** API boundaries, a schema/migration change, a config file — points where data enters or leaves the system delineate sequential phases. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design)

**Separate the bootstrap phase.** Scaffolding, shared types, CI/lint/test setup, a common base interface — this foundational work is inherently horizontal and every feature depends on it. Pull it into its own upstream node (the first wave); don't smear it across the feature nodes. [Work Breakdown Structure](https://en.wikipedia.org/wiki/Work_breakdown_structure)

---

## 2. Size each node to one coherent unit

Aim for the atomic sweet spot: big enough to deliver something demonstrable, small enough to implement and verify cleanly.

- **Shippability is the real test.** A node is correctly sized if its output can compile, pass its tests, and stand on its own without breaking the build. If it can't be shown working at the end of the run, it's either too small (a layer fragment) or sliced wrong. [Stacked Pull Requests](https://www.michaelagreiler.com/stacked-pull-requests/)
- **Rough upper bound.** Changesets past ~**400 lines** degrade review quality and hide defects; treat a node whose task clearly implies more than that as a candidate to split along a feature seam. [Pull Request Size Matters](https://bssw.io/items/pull-request-size-matters)
- **Rough lower bound.** If a node would be a sub-step of one capability ("create the model file", then "add validation", then "write the test"), setup and context cost dominates the actual work — fold it back into the capability node. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design)

Split anything that reads as *and* between unrelated capabilities; merge anything that's a fragment of one. (This is the SKILL.md "one focused job per node" rule applied to large work.)

---

## 3. Order by dependency, then parallelize into waves

Wire `depends_on` from real data/contract dependencies, then let independent nodes run concurrently.

| If… | Then… |
| --- | --- |
| Node A defines an API/type/schema and node B consumes it | **B `depends_on` A** — the contract must exist first. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) |
| Node A writes a migration and node B populates/queries it | **B `depends_on` A** — avoid schema mismatch. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) |
| Nodes touch disjoint domains with no shared files or types | **Independent** — same wave, run in parallel. [SPOQ](https://arxiv.org/html/2606.03115v1) |

- **Waves.** Group nodes with no mutual dependency into waves: wave 0 = bootstrap, wave 1 = features depending only on wave 0, and so on. The executor already runs a layer's nodes concurrently, so a clean wave structure is what buys parallelism. [SPOQ](https://arxiv.org/html/2606.03115v1)
- **Critical path.** The longest dependency chain sets the floor on wall-clock time; keep it short. Isolate high-risk or unfamiliar integrations (a third-party API, an unproven library) as their own early node so a failure there doesn't cascade. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design)

---

## 4. Multi-phase / multi-component: vertical slices, not horizontal layers

**Vertical slice (default).** One capability end-to-end through every layer it needs (data → logic → API → tests → UI). It delivers demonstrable value, exercises the whole path early, and matches WBS "work packages are deliverables, not activities". Prefer this for anything user-facing. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) · [Work Breakdown Structure](https://en.wikipedia.org/wiki/Work_breakdown_structure)

**Horizontal slice (the exception).** One layer across the whole app before the next. Only justified for: foundational platform work that must precede all features (the bootstrap node), a large migration/refactor where the whole codebase changes at once, or defining a shared interface that genuinely decouples many downstream nodes. [Pull Request Size Matters](https://bssw.io/items/pull-request-size-matters)

**Many components (a game with several mechanics, a dashboard of screens):**

1. Treat each mechanic / screen / module as its own high-level vertical slice → one node (or a small chain).
2. Slice vertically *within* it — e.g. character movement = model → state machine → render hook → input handler → tests, all in one node.
3. Parallelize across components that share **no files**: movement and inventory with no overlap go in the same wave. [SPOQ](https://arxiv.org/html/2606.03115v1)

A shared foundation (game loop, shared types, scaffold) is the wave-0 node every component `depends_on`.

---

## 5. Anti-patterns to reject

| Anti-pattern | Signal | Why it hurts |
| --- | --- | --- |
| **Over-splitting** | A capability chopped into "make model" → "add validation" → "write test" | Coordination and context-reload cost exceeds the work; each node re-learns the same context. [AI Development Patterns](https://github.com/PaulDuvall/ai-development-patterns) |
| **Catch-all node** | One node: "build the complete auth system" (UI + logic + schema) | Un-verifiable, un-mergeable, failures impossible to localize. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) |
| **Hidden dependencies** | Parallel nodes that quietly share a config file, global state, or the same source file | Race conditions and merge conflicts when concurrent nodes write the same files. [AI Development Patterns](https://github.com/PaulDuvall/ai-development-patterns) |
| **Horizontal fragments** | "add models" → "add repositories" → "add endpoints", none independently shippable | Big-bang integration at the end; nothing works until the last node. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) |
| **Phase skipping** | UI or integration node before its data model / contract exists | Cascading rework when the contract changes underneath it. [Atomic Task Design](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design) |

---

## When to split understanding from implementation

A `code-implementer` node already researches inline — it clones the repo into its own session and is told to study conventions and find a sibling feature before writing (see SKILL.md, "Write the implement node's task as research → plan → implement"). So the **default is ONE node**, understanding folded into its task — do NOT add a separate explorer node for a focused change in a conventional repo; that just makes the implementer re-learn context the feeder already paid for.

Split a standalone `code-explorer` feeder node → `code-implementer` only when **understanding the codebase is substantial work in its own right**, i.e.:

- the repo is large or unfamiliar enough that mapping how a feature is built is a real task before any code can be written correctly;
- **several** downstream implementer nodes share the same repo understanding (do the exploration once, feed all of them);
- the conventions are non-obvious and getting them wrong would mean rework across the whole slice.

When you do split, the explorer's report (structure, the sibling feature's full file set, conventions, wiring points) becomes the implementer node's brief — so the implementer still builds the complete vertical slice, just without re-deriving the map. One repo, one focused feature, conventional stack → keep it a single node.

---

## Decision procedure

1. **Parse** the request into distinct capabilities, domains, and user actions.
2. **Find seams** (§1); split out the bootstrap/shared-foundation phase as a wave-0 node.
3. **Draft one vertical node per capability** (§4) — end-to-end, not per layer.
4. **Size-check** each (§2): shippable and demonstrable on its own? Split the >400-LOC catch-alls; merge the sub-step fragments.
5. **Wire `depends_on`** from real contract/data dependencies only (§3); disjoint domains stay independent.
6. **Group into waves** (§3) and confirm parallel nodes in a wave share no files.
7. **Anti-pattern sweep** (§5): no cycles, no horizontal-only fragments, no hidden shared state, every feature has a testable vertical node.
8. **Route by capability** (SKILL.md rules): any node that changes/commits code is `code-implementer`; a node needing live web facts first is `web-researcher`; final aggregation is `synthesizer`.

---

*Grounded in a quack research synthesis on task decomposition. Sources: [Atomic Task Design (CodeSignal)](https://codesignal.com/learn/courses/task-decomposition-execution-with-claude-code/lessons/atomic-task-design), [Stacked Pull Requests (Greiler)](https://www.michaelagreiler.com/stacked-pull-requests/), [Pull Request Size Matters (BSSw)](https://bssw.io/items/pull-request-size-matters), [SPOQ (arXiv 2606.03115)](https://arxiv.org/html/2606.03115v1), [AI Development Patterns (Duvall)](https://github.com/PaulDuvall/ai-development-patterns), [Work Breakdown Structure (Wikipedia)](https://en.wikipedia.org/wiki/Work_breakdown_structure).*
