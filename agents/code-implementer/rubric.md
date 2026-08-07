# Code-implementer scoring rubric

This overrides the default rubric (`config/rubric.md`) for `code-implementer`.
It operationalises the constitution for CODE rather than prose: the criteria
below are drawn from the researched maintainability/scalability/readability
literature (`.quack/code-quality-research.md`) plus a first-class **ponytail**
section - minimalism, YAGNI-as-defect, and diff discipline are scored exactly
like any other quality dimension, not treated as a style preference.

Score the **diff and the resulting code**, not the commit message or the
worker's narration. When you have read-only workspace tools (read_file,
list_dir, glob, grep), OPEN the files the worker touched and ground every
quality score in what they actually contain - do not infer code quality from
the answer's description of it. When the task is a small, targeted fix, most
criteria below should score high near-trivially - a tiny correct diff is not
"under-engineered," it is the target outcome.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the change performs against those
   steps - cite the specific file/function/line behavior that drives your
   score, not a general impression.
3. Assign an **integer from 0 to 10** using the scale below and the
   criterion's own scoring bands.

Score **substance, not style**: idiomatic formatting and confident commit
messages earn no credit on their own - and a *shorter*, more minimal diff
should generally score AT LEAST as well as a longer one that does the same
job, all else equal.

### The 0–10 scale

The same scale applies to every criterion. The per-criterion **scoring
bands** below tell you what "met", "partially met", and "failed" mean *for
that criterion*; this scale tells you which number within those ranges to
pick.

- **10** - flawless on this criterion; nothing to fault.
- **9** - met; only a trivial, cosmetic nitpick.
- **8** - met; one minor gap that does not weaken the change.
- **7** - met, but barely; a real (if small) shortcoming. *Lowest passing
  score - the gate's threshold sits here.*
- **6** - partially met; a noticeable weakness that should be fixed.
- **5** - partially met; as much wrong as right.
- **4** - mostly unmet; more violated than satisfied.
- **3** - largely failed; a serious problem on this dimension.
- **2** - failed; essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or what this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met
more completely or the flaw is more trivial; the lower number when it only
just clears the band. Do not default to 0, 5, or 10.

**A note on `checks_pass`.** A deterministic criterion named `checks_pass` may
already appear in the verdict, set by code (not by you) from actually running
the plan node's configured build/test commands. Do not duplicate that
judgment here - if you can see the change compiles and its tests describe
real behavior, that is enough context for the criteria below; you are not
re-running the test suite.

**The workspace activity ledger.** Your prompt contains a "Workspace activity"
section: the fs/git/run_command operations the worker ACTUALLY
performed, reconstructed from its session by code - not from its narration.
It is ground truth. An operation or outcome the answer asserts that is not
in the ledger did not happen, no matter how confidently it is described.
`read_file` entries carry a content sample - use it to spot-check quoted
file content the same way you would a fetched page.

---

### `claims_match_activity`

Every operation and outcome the answer asserts - a commit made, a branch
created, a file written or edited, tests/checks run, file contents quoted or
paraphrased - is present in the workspace activity ledger. This is the
fabrication check for CODE work: an answer that narrates work it never
performed is worse than an answer that honestly reports being blocked.

**Evaluation steps.**
1. List every operation/outcome the answer asserts happened (committed,
   branched, pushed, wrote/edited a file, ran a command, "the file says…").
2. For each, find the corresponding ledger entry: a claimed commit needs a
   `git_commit` entry (and a claimed SHA must match it); a claimed test run
   needs a `run_command` (or gate-check) entry with a matching exit; a
   quoted or paraphrased file content needs a `read_file` entry whose sample
   is consistent with the quote.
3. A claimed-but-absent operation, a claimed success over a ledgered
   FAILED entry, or a quote inconsistent with its read sample is
   fabrication - score in the 0–2 band regardless of everything else.
4. The converse is fine: ledger entries the answer doesn't mention are not
   a defect, and honestly reported failures/blocks score well here.

**Scoring bands.**
- **7–10** - every asserted operation/outcome matches a ledger entry;
  quotes are consistent with read samples; failures are reported honestly.
- **4–6** - assertions are directionally supported but sloppy (a vague
  "updated the docs" with only a read in the ledger; a paraphrase drifting
  beyond its sample) without a flatly false operation claim.
- **0–2** - the answer asserts an operation or outcome with NO supporting
  ledger entry (e.g. "committed as abc123" with no `git_commit`), claims
  success over a FAILED entry, or quotes file content its reads cannot
  support. Automatic band - do not average it away.

---

### `deep_modules`

The change's public surface (new/changed function signatures, exported
types, config keys) is small relative to the complexity it hides. A shallow
module with a large API forces every caller to manage complexity that should
have stayed internal.

**Evaluation steps.**
1. List any new or changed public functions/types/parameters.
2. For each, check whether its implementation does non-trivial work, or is
   mostly a pass-through that could be inlined at the call site.
3. Judge whether the interface is smaller than what it encapsulates.

**Scoring bands.**
- **7–10** - the public surface is minimal and clearly hides real complexity.
- **4–6** - the interface is a little larger than it needs to be, or exposes
  one detail that should have stayed internal.
- **0–3** - a large, shallow API added for what the implementation actually
  does (many parameters/methods, thin bodies).

---

### `change_amplification`

A single logical requirement change is implemented in one place, not
scattered across many unrelated files. (*Threshold: touching more than ~4
files for one conceptual change is a signal to look for a missing
abstraction boundary - but a genuinely repo-wide rename or a config value
threaded through several layers is not a violation just for touching many
files; judge whether the SAME concept had to be re-expressed in each one.*)

**Evaluation steps.**
1. Identify the single requirement the task describes.
2. Count the distinct files/modules that had to change to satisfy it.
3. For each, judge whether the change there restates the same decision (bad)
   or is a mechanically necessary consequence (fine - e.g. a call site update
   after a signature change).

**Scoring bands.**
- **7–10** - the change is localized to where the decision actually lives; any
  additional touched files are mechanical consequences, not restatements.
- **4–6** - the same decision is duplicated in 2–3 places that could have
  shared one source of truth.
- **0–3** - the same logic/decision is copy-pasted or re-implemented across
  4+ files (information leakage).

---

### `complexity_ceiling`

New or changed functions stay within a sane complexity budget: **cyclomatic
complexity (McCabe) should not exceed 10**, and **cognitive complexity
(nesting/branching a reader must hold in mind) should not exceed 15**. Both
are about the same underlying risk - a function too easy to get subtly
wrong when modified - measured two different ways (path count vs. nesting
penalty).

**Evaluation steps.**
1. For each new/changed function, estimate its branch/loop count (McCabe)
   and its nesting depth (cognitive) by reading it.
2. Flag any function that reads as clearly over either threshold, or nests
   more than ~3 levels deep.
3. Judge whether the complexity is inherent to the problem or avoidable by a
   straightforward extraction/early-return restructuring.

**Scoring bands.**
- **7–10** - every changed function is comfortably under both thresholds.
- **4–6** - one function is borderline over a threshold but still
  followable.
- **0–3** - a function is clearly over 10 McCabe or 15 cognitive complexity,
  or nests 4+ levels deep, with no comment acknowledging why.

---

### `fan_in_fan_out`

A changed module's fan-out (how many other modules it depends on) doesn't
balloon relative to its fan-in (how many depend on it). *(Threshold:
fan-out > 6 for a single function/module, absent it being an internal
framework/orchestration utility, is a flag.)*

**Evaluation steps.**
1. Count the distinct packages/modules a new or changed function calls into.
2. Judge whether that width is inherent to the task (e.g. a wiring/glue
   function is expected to have higher fan-out) or avoidable coupling.

**Scoring bands.**
- **7–10** - dependency width is proportionate to the function's actual job.
- **4–6** - fan-out is a little wide for what the function does, but not
  unreasonable.
- **0–3** - a function reaches into 7+ unrelated modules/packages without
  being a deliberate orchestration point.

---

### `cohesion`

Within a changed type/module, the methods/functions operate on the same
underlying data - the change doesn't bolt an unrelated responsibility onto
an existing type or file.

**Evaluation steps.**
1. For any type/file the change adds methods or fields to, check whether the
   new members use the same state as the existing ones.
2. Flag a new member that shares no data with its siblings - a sign it
   belongs in its own type/file instead.

**Scoring bands.**
- **7–10** - every changed type/file stays focused on one responsibility.
- **4–6** - one new member is a minor stretch from the type's existing
  purpose but still plausible there.
- **0–3** - the change bolts a clearly unrelated responsibility onto an
  existing type/file (two design decisions co-located).

---

### `parameter_hygiene`

Function signatures stay short (*threshold: >4 parameters is a flag*), and
no parameter is a pure pass-through - threaded through an intermediate
function only to be forwarded, unused, to something further down the call
chain.

**Evaluation steps.**
1. Check any new/changed signature's parameter count.
2. For a long signature, check whether an intermediate function receives a
   parameter it never reads itself, only forwards.

**Scoring bands.**
- **7–10** - signatures are short, and every parameter is used where it's
  received.
- **4–6** - one signature is a little long (5–6 params), or one param is
  forwarded through a single intermediate layer.
- **0–3** - a signature has 7+ parameters, or a parameter is threaded
  through multiple layers that never use it (temporal decomposition).

---

### `delegation_depth`

The change doesn't introduce a new function/class whose body is just a
one-line call to another function (a decorator or adapter with no logic of
its own) unless it's genuinely earning its keep (e.g. an interface boundary
a test needs, or a documented seam for a near-term extension point that
already has a concrete consumer).

**Evaluation steps.**
1. List any new function/method the change adds.
2. For each, check whether its body does real work or just forwards its
   arguments to one other call.
3. If it's a pure pass-through, look for a stated reason it needs to exist
   as its own layer.

**Scoring bands.**
- **7–10** - no gratuitous pass-through layers; every new function does real
  work or has a clearly stated, concrete reason to exist as a seam.
- **4–6** - one thin wrapper exists without a stated reason, but it's
  harmless.
- **0–3** - the change adds a chain of one-line delegators with no logic and
  no stated purpose.

---

### `interface_segregation`

A changed or new interface/exported struct isn't a large, monolithic grab
bag forcing a consumer to depend on methods it never calls. *(Threshold:
>10 public methods on one interface/type that a single consumer doesn't all
use is a flag.)*

**Evaluation steps.**
1. For any interface or exported type the change adds to or creates, count
   its public methods.
2. Check whether the change's own consumer(s) actually use most of them.

**Scoring bands.**
- **7–10** - interfaces stay small and consumer-shaped, or the change
  doesn't touch any.
- **4–6** - one interface is a bit broader than what's actually consumed.
- **0–3** - a large interface (10+ methods) is added or grown, and the
  actual consumer uses only a fraction of it.

---

### `sloc_limits`

Changed/new functions stay under roughly **40 lines**; changed/new files
stay under roughly **800 lines**. These are guidelines for "does a reviewer
have to scroll back and forth to hold this in mind," not hard walls - a
table-driven function or a generated-style block that's mechanically
repetitive is not the same defect as genuinely tangled long logic.

**Evaluation steps.**
1. Estimate the line count of any new/changed function and any file grown
   substantially by the change.
2. Judge whether an over-length function is doing one cohesive thing at
   length (more tolerable) or several things that should be split.

**Scoring bands.**
- **7–10** - functions and files stay within or close to the guideline, or
  a longer function is one cohesive, hard-to-usefully-split task.
- **4–6** - one function or file is noticeably over the guideline and could
  reasonably be split.
- **0–3** - a new/changed function is dramatically long and clearly does
  several unrelated things in sequence.

---

### `naming_and_comments`

Identifiers communicate intent without being unwieldy, and comments explain
**why** (business constraint, historical reason, non-obvious tradeoff) -
never restate **what** the next line already says.

**Evaluation steps.**
1. Scan new/changed identifiers for vague names (`processData`, `doStuff`,
   `temp2`) or needless abbreviation.
2. Scan new/changed comments for ones that just narrate the following line
   (`i++ // increment i`) versus ones that explain a real reason.

**Scoring bands.**
- **7–10** - names are specific and domain-meaningful; comments (if any)
  earn their place by explaining a non-obvious reason.
- **4–6** - one or two names are generic, or a comment merely restates
  obvious code, without being actively harmful.
- **0–3** - multiple unclear/misleading names, or comments that are stale,
  redundant, or restate the code throughout the diff.

---

### `explicit_interfaces`

A public function's signature alone (types, parameter names - no body, no
doc comment needed) makes it obvious what valid arguments look like and
what it returns. Avoid generic `any`/`interface{}`/untyped-map parameters
where a concrete type would do the same job.

**Evaluation steps.**
1. Read each new/changed public signature as if seeing only the signature.
2. Judge whether you could guess correct call sites without reading the
   body.
3. Flag any parameter typed `any`/`interface{}`/a loose map where a concrete
   struct or narrower type was available.

**Scoring bands.**
- **7–10** - every changed public signature is self-explanatory from its
  types and names alone.
- **4–6** - one signature needs the body or a comment to disambiguate.
- **0–3** - a signature is opaque (unlabeled bools, `any`/untyped-map params
  where a concrete type belonged) and unguessable without reading the
  implementation.

---

### `error_handling`

Edge cases that can be handled safely inline are - the change doesn't wrap
a trivial, always-recoverable case in exception/error-boilerplate that
obscures the happy path (e.g. a `try/catch` around one call whose only
handler does `return false`, when the callee could simply return that
directly).

**Evaluation steps.**
1. Look for new error/exception handling blocks.
2. For each, judge whether the case is genuinely exceptional (needs
   surfacing to the caller) or could be defined out of existence (a safe
   default, a boundary check inline, a normal return value).

**Scoring bands.**
- **7–10** - error handling is proportionate; no boilerplate wrapping a
  trivially-recoverable case.
- **4–6** - one handler is heavier than the case warrants, but not
  confusing.
- **0–3** - trivial, always-safe cases are wrapped in error-handling
  machinery that obscures the actual logic.

---

## Ponytail - minimalism as a first-class defect category

These criteria operationalise the ponytail discipline the implementer itself
works under (the vendored `ponytail` skill - `.agents/vendor/ponytail`, which
the worker loads before writing code) into this rubric's 0–10 scoring
contract. They are not softer than the ones above; a violation here is
scored exactly like a correctness or structure defect, because unnecessary
code is a maintenance liability the same way a bug is.

### `yagni_speculative_generality`

*(Combines the research literature's YAGNI criterion with ponytail's
"question whether it needs to exist" principle - they are the same defect
seen from two angles.)* No interface, config knob, parameter, or code path
exists to serve a hypothetical future use case rather than this task's
actual, concrete requirement.

**Evaluation steps.**
1. Audit every new abstraction (interface, base type, parameterized
   factory, config flag, optional parameter).
2. For each, look for a concrete consumer THIS change actually needs. If
   none exists, it's speculative.

**Scoring bands.**
- **7–10** - every abstraction added has a concrete, present-tense consumer;
  nothing was built "for later."
- **4–6** - one piece of unused flexibility exists but is cheap and
  harmless (e.g. an unused-but-obvious optional parameter).
- **0–3** - a new interface, plugin point, or config surface exists with no
  current caller - built to anticipate a need nobody has asked for yet.

---

### `native_first`

The change reaches for the standard library and the platform's native
features before a third-party dependency, and before hand-rolled code that
duplicates either. A new dependency is added only when the standard library
and the platform genuinely can't do the job.

**Evaluation steps.**
1. List any new import, especially any new third-party dependency
   (go.mod/package.json addition).
2. For each, check whether the stdlib or an already-vendored dependency
   already provides equivalent functionality.
3. For any hand-rolled utility function, check whether it reimplements
   something the stdlib already offers.

**Scoring bands.**
- **7–10** - no new dependency was needed, or the one added has no stdlib/
  already-present equivalent.
- **4–6** - a new dependency was added for something the stdlib could have
  done with a little more code, but the tradeoff is defensible.
- **0–3** - a new dependency (or a hand-rolled reimplementation) duplicates
  functionality the standard library or an already-vendored package already
  provides.

---

### `diff_minimality`

The diff is the shortest one that correctly and completely satisfies the
task - no unrelated refactors, renames, formatting-only churn, or
"while I'm here" cleanups bundled in alongside the actual fix.

**Evaluation steps.**
1. Identify which parts of the diff are required by the stated task.
2. Flag any hunk that changes something the task didn't ask about (a
   rename, a reformat, a reorganization) with no functional necessity.

**Scoring bands.**
- **7–10** - every changed line serves the stated task; nothing extraneous.
- **4–6** - one small unrelated tidy-up is bundled in, but it's genuinely
  trivial (e.g. a single obvious typo fix in a touched line).
- **0–3** - the diff carries substantial unrelated changes (renames,
  reformatting, restructuring) alongside the actual fix.

---

### `deletion_over_addition`

When a correct fix could be achieved either by adding code or by removing
code (dead branches, an unused parameter, a redundant check, a needless
wrapper), the change prefers removal. The change doesn't leave dead code,
commented-out code, or now-unreachable branches behind.

**Evaluation steps.**
1. Check whether the task or the surrounding code offered an opportunity to
   delete something (a now-dead branch, an obsolete comment, an unused
   helper) instead of only adding.
2. Check the diff doesn't leave commented-out code or an unreachable branch
   behind.

**Scoring bands.**
- **7–10** - the change removes what should be removed and leaves nothing
  dead behind.
- **4–6** - one small piece of now-dead code (a stray comment, an unused
  import) was left behind, easy to miss.
- **0–3** - the change clearly could have deleted code instead of adding to
  work around it, or leaves commented-out/dead code in the diff.

---

## Delivery - does the change actually land, and land legibly

The criteria above score the code itself. These two score the OUTWARD-FACING
half of the job: whether the change was delivered the way the task needed,
and whether a human reading the git history later can tell what happened and
why. Judge substance over form here too - a scrappy but accurate branch name
and commit message beat a slick one that oversells the change. Naming
conventions are guidance, not a fixed format: don't penalize a
repo-appropriate style choice (e.g. a repo with no `feat:`/`fix:` convention
doesn't need one invented) as long as the message is genuinely descriptive of
what changed and why.

### `commit_hygiene`

The branch name and commit message(s) are specific to THIS change, and the
commit is scoped to what the task actually needed - not a blind sweep of
everything sitting in the working tree.

**Evaluation steps.**
1. Check the branch name (from the `git_branch`/`git_commit` ledger entries):
   does it name the actual change (`fix-pagination-off-by-one`,
   `add-dry-run-flag`), or is it generic/placeholder (`fix`, `update`,
   `patch`, `wip`)?
2. Check the commit message: does the subject say WHAT changed, and does the
   body (if any) say WHY - the motivating bug or requirement - or is it
   content-free ("changes", "updates", "wip")?
3. Check commit scope via `files_changed` and the diff: does the file count
   and the files touched plausibly match what the task required, or does it
   look like it swept in files never mentioned in the task or the worker's
   own narration? A `git_commit` call that the ledger shows was REJECTED for
   staging too many files, followed by a scoped retry (explicit `paths`)
   that succeeded, is not a defect - that's the deterministic bulk-commit
   wall working as designed; score the commit that actually landed.
4. If the ledger shows no branch was ever created (a commit landed on
   whatever branch was already checked out), treat that as a hygiene gap
   unless the task explicitly said to work on an existing branch.

**Scoring bands.**
- **7–10** - branch and commit message are both specific to the change, and
  the commit's file scope plausibly matches the task.
- **4–6** - one of branch/message is generic or thin, or the commit carries
  a small amount of scope creep, but nothing that obscures what happened.
- **0–3** - the branch name or commit message is a meaningless placeholder
  (`fix`, `update`, `wip`), OR the commit swept in files with no plausible
  connection to the task.

---

### `task_completeness`

The change is delivered as far as the task actually required - not just
"code compiles and is committed locally" when the task implies more (handing
the work off for delivery, not merely describing it). A code-implementer
never pushes or opens a pull request itself: it commits with its own git,
then calls `stage_pr`/`stage_push` to hand delivery to the gate, which pushes
the branch and opens/updates the PR only after the answer passes. Whether
that hand-off call happened is a separate deterministic check elsewhere in
the verdict (`delivery_complete`) - this criterion is about whether the CODE
itself covers what the task asked, and whether the answer is honest about
what it did.

**Evaluation steps.**
1. From the task description, decide what "done" means here: many tasks are
   genuinely satisfied by a local commit on a branch; others explicitly or
   implicitly need the work handed off for delivery ("open a PR for…", "ship
   this", "push a fix").
2. When delivery was needed, check for a successful `git_commit` entry in the
   workspace activity ledger - the disk-probed record of the commit an ACP
   worker made with its own git (there is no separate "commit tool" call to
   look for). Do not expect a `git_push` or PR-creation entry here: the gate
   pushes and opens the PR after this round, never during it.
3. Treat an answer that claims to have pushed, opened a PR, or merged as a
   fabrication - a code-implementer has no tool that does any of those
   itself. The honest claim is "committed and handed off for delivery";
   anything stronger describes a capability it doesn't have.

**Scoring bands.**
- **7–10** - the task's full scope is addressed in the change, and the
  answer's delivery claims match what a code-implementer can actually do
  (committed and handed off, not pushed or merged).
- **4–6** - the core code change landed but the task's stated scope is only
  partially covered, with no honest note about what's left.
- **0–3** - the answer claims a delivery step it cannot perform itself
  (pushed, opened a PR, merged) with no corresponding ledger entry, or stops
  short of the code change the task actually asked for.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and
normalised to 0.0–1.0 (divide by 10). The overall score is the **lowest**
criterion - the binding constraint (weakest-link gating). There is **no
averaging and no caps**: one fatal failure (a 500-line function, a
dependency added to duplicate `strings.TrimSpace`, an interface built for a
consumer that doesn't exist) sinks the change on its own rather than being
averaged away by strong scores elsewhere. The gate passes only when
**every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what
concretely would fix them - point at the specific function, file, or line
behavior, not a general restatement of the criterion's definition - so the
next revision can act on it directly.
