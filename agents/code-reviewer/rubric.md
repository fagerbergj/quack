# Code-reviewer scoring rubric

This overrides the default rubric (`config/rubric.md`) for `code-reviewer`. It
scores the quality of a REVIEW, not the quality of the code being reviewed:
did the review catch the change's real problems, weight them by impact, stay
constructive and actionable, keep nits from blocking, and verify claims rather
than assert them.

Score the **review the worker produced** - its findings, their severity
labels, and its verdict. When you have read-only workspace tools (read_file,
list_dir, glob, grep) and the reviewed change is in the workspace, OPEN the
files the review discusses and check its findings against what the code
actually contains - do not take the review's word for the code's behavior. A
review of a small, clean change that correctly finds little and approves is
not "shallow"; a short accurate review is the target outcome.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the review performs against those
   steps - cite the specific finding (or missing finding) that drives your
   score, not a general impression.
3. Assign an **integer from 0 to 10** using the scale below and the
   criterion's own scoring bands.

### The 0–10 scale

- **10** - flawless on this criterion; nothing to fault.
- **9** - met; only a trivial nitpick.
- **8** - met; one minor gap that does not weaken the review.
- **7** - met, but barely; a real if small shortcoming. *Lowest passing score
  - the gate's threshold sits here.*
- **6** - partially met; a noticeable weakness that should be fixed.
- **5** - partially met; as much wrong as right.
- **4** - mostly unmet.
- **3** - largely failed.
- **2** - failed; essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or what this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met
more completely; the lower when it only just clears the band. Do not default
to 0, 5, or 10.

**The workspace activity ledger.** Your prompt contains a "Workspace activity"
section: the read-only fs/git operations the reviewer ACTUALLY performed,
reconstructed from its session by code - not from its narration. It is ground
truth. `read_file`/`git_diff` entries carry a content sample; use them to
spot-check whether a finding refers to code the reviewer actually looked at.

---

### `claims_grounded`

Every finding the review makes is grounded in code the reviewer actually read
- it points at a real file/line/behavior, not an invented defect. This is the
fabrication check for a review: asserting "this function swallows the error"
about code the reviewer never opened is as disqualifying as an invented
citation.

**Evaluation steps.**
1. List the review's substantive findings (each blocking issue, suggestion,
   and factual claim about the code's behavior).
2. For each, check it against the ledger's read/diff samples and, if you can,
   the file itself: does the cited code actually behave the way the finding
   says?
3. A finding about code contradicted by the file, or asserting behavior in a
   file the reviewer never read, is fabrication - score in the 0–2 band.

**Scoring bands.**
- **7–10** - every finding refers to code that exists and behaves as claimed;
  findings cite where they live.
- **4–6** - findings are directionally right but loosely grounded (a vague
  "error handling looks off somewhere" without a location).
- **0–2** - a finding describes behavior the code does not have, or critiques
  a file the ledger shows the reviewer never opened. Automatic band.

---

### `catches_real_issues`

The review surfaces the change's actual defects - the correctness, security,
and missing-test problems that matter - rather than only cosmetic ones. Judge
against what the change actually contains, not against an imagined ideal
review.

**Evaluation steps.**
1. From the diff, identify the genuine problems in the change (bugs, unhandled
   edge cases, security exposure, new behavior with no test).
2. Check whether the review found each, or missed a significant one while
   spending its attention on trivia.

**Scoring bands.**
- **7–10** - the review catches the change's real, impactful issues (or
  correctly finds few, because the change is clean).
- **4–6** - it catches some but misses one material issue, or buries it among
  minor points.
- **0–3** - it misses the change's central defect, or reports only cosmetic
  findings while a real bug/security/test gap goes unmentioned.

---

### `severity_grounded`

Every severity label rests on something objective, and the review says what.
A finding supported only by the reviewer's preference is a `nit:`, however strongly it is argued.

`blocking:` requires one of five anchors:
**defect** (concrete inputs or state produce wrong output, a crash, or data loss),
**security**,
**design** (a NAMED principle violated, not "I'd have done it differently"),
**scope** (the change does what its task never asked for),
or **tests** (new behaviour with no test, or a test that passes while what it claims to cover is broken).

Score whether each label is *supported*, not whether you would have chosen it.
Two reviewers can reasonably weigh the same finding differently; neither may block on taste.
The style guide and the linter are the authority on style, and out-of-scope observations belong in their own issue rather than gating this change.

**Evaluation steps.**
1. For each `blocking:` finding, name which of the five anchors it claims, then open the code and check the anchor actually holds.
   An anchor asserted but not borne out by the code means the label is wrong, however serious the finding sounds.
2. For each `suggestion:`/`nit:`, check it is not one of the five mislabelled downward.
   A defect soft-pedalled into a suggestion is the more damaging error, because it ships.
3. Check nothing gates the merge on preference: is any `blocking:` finding one the author could reasonably decline at no objective cost?

**Scoring bands.**
- **7–10** - every label carries a supported anchor; nothing blocks on taste.
- **4–6** - one label unsupported, or one defect soft-pedalled below `blocking:`, or one preference treated as blocking.
- **0–3** - labels are preference-driven, or a defect/security finding is labelled below `blocking:` while style gates the merge.

---

### `constructive_actionable`

The review critiques the work, not the developer; explains the *why* behind
each finding so it's actionable. Language is plain and respectful - no
sarcasm, hyperbole, or diminishing words. A finding that proposes a SPECIFIC
code change is only actionable if it shows that code, not just describes it.
A summary that also includes a sincere piece of praise is a polish signal,
worth at most a point off if missing - it can never by itself pull this
criterion below passing.

**Evaluation steps.**
1. Check the tone targets the code, not the author, and avoids "just"/"always"
   /"obviously"-style diminishment.
2. Check findings explain the underlying reason (risk/principle/benefit), not
   only "this is wrong".
3. Note whether the summary - `stage_review`'s body, or the reply text ahead
   of the fallback `VERDICT:`/`FINDINGS:` tail - includes at least one
   genuine, sincere sentence of praise. There is no `praise:` label; the
   prompt reserves praise for the summary. Its absence is a minor deduction,
   never a disqualifier.
4. For each finding that proposes a specific code change (a rename, an
   extraction, a different implementation - anything more concrete than "this
   could be cleaner"), check whether it supplies that code in a fenced code
   block with a language tag, rather than only describing the change in
   prose. A PURELY OBSERVATIONAL finding - a question, a naming nit with no
   proposed name, "consider whether this handles X" - proposes nothing
   concrete and needs NO code block; do not penalize those for lacking one.

**Scoring bands.**
- **7–10** - respectful, every finding explains its why, and every finding
  proposing specific code supplies it in a fenced block. Missing praise costs
  at most one point here (e.g. 10 → 9), never more.
- **4–6** - actionable but terse (some findings state what without why), or a
  finding proposes a specific change in prose without the code to back it.
- **0–3** - comments target the person, are dismissive, or are unactionable
  ("this is bad") with no reasoning.

---

### `verification_over_assertion`

The review verified the change's claims rather than trusting them - it checked
whether the code does what its description says and whether the tests would
actually fail if the code were broken, instead of rubber-stamping.

**Evaluation steps.**
1. Check whether the review engaged with the change's stated intent and
   confirmed the diff actually delivers it.
2. Check whether it assessed the tests' meaningfulness (do they exercise the
   new behavior / failure modes) rather than just noting tests exist.

**Scoring bands.**
- **7–10** - the review verifies the change's claims and interrogates the
  tests' substance.
- **4–6** - it verifies the code but takes the tests at face value (or vice
  versa).
- **0–3** - it rubber-stamps: accepts the description and the presence of tests
  without checking either.

---

### `structured_verdict`

The review is delivered in a usable shape - a summary (praise belongs only
here), then findings grouped by severity (blocking → suggestions → nits) -
and carries a clear, consistent verdict. The verdict is STRUCTURED DATA, not
prose: the reviewer stages it via `stage_review`'s `event` (or the answer's
`VERDICT:` tail), and
your prompt surfaces the resolved value as "Staged review verdict: <event>"
when one is staged. Score presence and consistency of THAT, never whether the
summary restates it in words - the reviewer is deliberately told the summary
is a fifteen-second takeaway that does NOT repeat the verdict.

**Evaluation steps.**
1. Check the review opens with a high-level summary and groups findings by
   severity rather than scattering them.
2. Read the staged verdict provided to you and check it is present and consistent with the findings.
   Three verdicts, three levels of confidence: any surviving `blocking:` ⇒ `request_changes`; else any unresolved `question:` ⇒ `comment`; else ⇒ `approve`, nits and suggestions okay.
3. `comment` is legitimate ONLY when an unresolved `question:` stands, or the review states verification it could not finish.
   A question withholds approval without demanding a change - the reviewer works from a bounded context, so not understanding something is at least as likely to mean it is missing a file as it is to mean the code is wrong.
   What fails here is a bare `comment` used to avoid committing to a verdict with nothing unresolved behind it, or no staged verdict at all.
4. Cross-check every finding's OWN severity label against the overall
   verdict - the two must tell the same story. A finding labeled `blocking:`
   (with or without a category tag like `blocking (security):`) sitting
   alongside an `approve` verdict is a direct self-contradiction, regardless
   of whether the underlying issue actually warrants blocking; so is a
   `request_changes` verdict backed by nothing but `nit:`/`suggestion:`
   findings. This is a coherence check on the review's OWN labels, not a
   re-review of the diff - reason about it, do not pattern-match on the label
   text alone.

**Scoring bands.**
- **7–10** - clear summary, severity-grouped findings, a present and
  consistent staged verdict (including an explicit approve on a clean change).
- **4–6** - mostly structured but missing the summary, or the staged verdict
  slightly mismatches the findings.
- **0–3** - an unstructured wall of comments with no summary, no staged
  verdict at all, a staged verdict that contradicts the findings (approves
  over an unresolved blocking issue, or ANY finding is labeled blocking/
  security-severity while the verdict is `approve`, or `request_changes` is
  staged over findings that are all nits/suggestions), or a clean review
  resolved to `comment` instead of `approve`.

---

### `signal_over_noise`

The review reads as findings for a human deciding whether to merge, not a
transcript of the reviewer's own process - and it never turns an
environment-specific failure into a code-quality concern.

**Evaluation steps.**
1. Check the visible body (outside any collapsed `<details>` block) for
   process narration - a "What I ran"/"What I checked" list of commands
   (`git diff`, `go test`, `gofmt`, `go vet`, install steps) presented as
   review content rather than tucked away for debugging.
2. Check every reported test/build failure or "pre-existing issue": is it
   tied to the diff, or is it a sandbox/toolchain/network gap (missing
   `make`, no network, stale cache) that the PR's own green CI contradicts?
   A failure that doesn't reproduce in CI and isn't caused by the diff is not
   a legitimate finding.

**Scoring bands.**
- **7–10** - the body is findings and verdict only (process notes, if any,
  are in a collapsed block), and no environmental-only failure is reported
  as a concern.
- **4–6** - a process-narration section precedes the findings, or one
  reported failure is questionable but not clearly environmental.
- **0–3** - the review leads with "what I ran" as its substance, or reports a
  sandbox/environment failure as a code concern on a PR whose CI is green.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised
to 0.0–1.0 (divide by 10). The overall score is the **lowest** criterion - the
binding constraint (weakest-link gating). There is **no averaging and no
caps**: one fatal failure (a fabricated finding, a missed central bug, blocking
the merge on a nit) sinks the review on its own. The gate passes only when
**every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely
would fix them - point at the specific finding (or the missing one), not a
restatement of the criterion - so the next revision can act on it directly.
