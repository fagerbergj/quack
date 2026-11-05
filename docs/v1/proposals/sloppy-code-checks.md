# Proposal: sloppy-code checks for the reviewer and the implementer

- **Status:** proposal for the owner, 2026-11-05. Nothing here ships until the owner signs it off and each change is measured as described below.
- **Touches:** the reviewer's prompt (`v1/agents/reviewer/prompt.md`) and `github.reviewer` rubric (v5 as narrowed by #2188), its evidence builder (`v1/packages/github/src/review.ts`), the implementer's `deliver` checks (`v1/packages/github/src/task.ts`), and the implementer rubric that [code-work.md](../audits/code-work.md) proposes.
- **Judge rules this follows (owner, 2026-11-05):**
  - No check runs an open-ended search, where there is nothing to list, like "is there any defect anywhere?" (#2188 removes `no_silent_approval` and `reports_every_defect`).
  - A universal claim over a set that code can list is fine when it fans out one positive question per item, as `promises_handled` and `sibling_parts` will.
  - Chunking and fan-in are declarations, never bespoke code.
- **Evidence:** reports slop1-slop5, with the claims used here checked as listed in [Sources checked](#sources-checked).

## Indicators, ranked by harm times provability

"Provable" means a reviewer or a gate can show it from the diff and a run, without a judgement call about intent.

| # | Indicator | Definition and example | What a finding cites | Mechanical? | False positives | Harm |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | **Test passes without the change** | A new or changed test is green on the base commit, so it does not test the change. A fix PR adds `expect(fmt(0)).toBe('0')`, which the old code already returned. | The test file and its result on base | Yes: run the test with base sources | New tests for behavior that already worked (refactors, pinning tests); compile-only failures count as red | High: the tests assert nothing about the change, but CI shows green |
| 2 | **Focused, skipped or retried tests** | `.only`, `{ only: true }`, a new `.skip`, `retries`, `--reruns`, a CI `retry:` | The line | Yes: a token lint over added lines | Near zero for `.only`; low for skips that cite an issue | High: `.only` silently drops the rest of the suite, and retries hide real failures |
| 3 | **Tautological assertion** | The expected value comes from the test's own stub, from the code under test, or is the same expression as the actual. `stub.returns(42); expect(svc.get()).toBe(42)` | The arrange line and the assert line | Partly: a lint for self-comparison and stub-equals-expected; otherwise judged per test | Interaction tests that legitimately check call arguments | High: green over any bug |
| 4 | **No assertion executes** | The test body has no assertion, or its only assertion sits in a loop over a possibly empty list, a catch block, or a mock's body | The test body | Partly: no assertion call is lint-detectable; the rotten-green forms need a judgement or a run | Smoke tests that only check nothing throws | High |
| 5 | **Flake source in a test** | A fixed sleep after async work, a wall-clock read that feeds an assertion, an unseeded random value, a real network host, shared state with no teardown | The line plus the assertion it feeds | The token is lint-detectable; whether it decides the outcome is judged | Sleeps inside a polling loop with a timeout, a timer that is itself the subject, stopwatch reads with slack | High: async waits are the largest flaky category (Luo et al., 45% of classified fixes) |
| 6 | **Copy-paste clone** | A block of the diff repeats code elsewhere in the repo (exact or renamed identifiers) | Both file:line ranges | Yes: token clone detection (jscpd) | Low for exact clones; renamed clones, tests and data tables are noisier | Medium: a fix applied in one copy is missed in the other |
| 7 | **Second helper beside an existing one** | A new function does the job of one the repo already exports. `formatMoneyCents()` beside `formatCurrency()`. | The new function and the existing one | Candidates by code (name tokens and clone similarity); the match is judged | Deliberately different policies | Medium-high: drift between the two copies |
| 8 | **Leak across a package boundary** | Core imports one specific extension, or a public signature, config key or error names the internal vendor. `QueueInterface.getChannel(): RabbitMQChannel` | The import, or the public symbol | Imports: yes (quack's own `tools/boundary.ts`); names: judged with the substitution test | Names of dependencies the operator chose (a user-facing `REDIS_URL`) | Medium: callers come to depend on the internals |
| 9 | **Swallowed error** | An empty catch, or a catch that returns `null` or `[]` | The catch block and the failure it hides | Empty catch: lint; return-empty: judged | Documented suppressions | Medium-high |
| 10 | **Hallucinated API or package** | A call, option or dependency that does not exist in the installed version | The call and the lockfile version | Calls: typecheck and CI; packages: a registry lookup | Near zero for a missing package | High, but CI already catches most of it |

Rows 1, 2 and 6 are deterministic gates for the implementer. Rows 3-5 and 7-9 are judged checks, each over a listed set. Row 10 stays with CI and typecheck.

## Implementer: deterministic gates first

Luo et al. found "most flaky tests (78%) are flaky the first time they are written", so the commit that adds a test is the cheapest place to catch it. Mark Seemann's rule for tautologies points the same way: "If you haven't seen it fail, you don't know if you've avoided a tautology."

These gates run inside `deliver`'s `problems()` in the session sandbox, before the human approval. A failure returns "Not delivered; fix these" with each item, exactly as the commit-subject check does today. They need no model.

### G1 `new_tests_fail_on_base`

- **Rule.** Every test file the delivery adds or changes must fail on the base sources. A file that passes there fails the gate.
- **Which files are tests.** Changed files matching the repo's test globs (`*.test.*`, `*_test.go`, `test_*.py`, `*_spec.*`, `**/tests/**`). A repo can override the globs in its conventions, since jinja's `src/jinja2/tests.py` is production code.
- **Which command.** The test commands under "How verified" in `artifacts/pr.md` that the session ran green. `claimedCommands` and `sessionRuns` already collect them. The command is narrowed to the file through one runner adapter each: `node --test <file>`, `go test ./<pkg>`, `pytest <file>`, `vitest run <file>`.
- **How it runs.**
  1. `git checkout <base> -- <changed non-test files>` in `./repo`.
  2. Run the narrowed command per test file with a 5 min timeout.
  3. `git checkout HEAD -- .` to restore the head sources.
  - Using the same tree keeps `node_modules` and workspace links valid. A separate worktree with borrowed `node_modules` would resolve workspace packages to the head tree and test head code. If `package.json` or the lockfile changed, it reinstalls first.
- **Exemptions.**
  - The delivery's commits are all `refactor:` or `test:`.
  - `artifacts/pr.md` has a "Pass on base" line naming the file and the reason, which the approver reads.
  - A timeout is reported as a timeout, never as a pass.
- **Limit.** A test that fails on base only because it imports a new symbol passes the gate. G2's assertion rule and the judged check J2 cover that case.
- **Cost.** One extra test run per changed test file. That is seconds for `node --test` on one file and minutes at worst for a whole Go package. It uses no model.

### G2 `test_diff_lint`

A token lint over added lines in test files and CI config. Hard failures:

- **Focused test:** `.only(`, `only: true`, `fit(`, `fdescribe(`, `test.only`.
- **Newly skipped test** without `#<issue>` on the same line: `.skip(`, `skip: true`, `xit(`, `t.Skip(`, `@pytest.mark.skip`.
- **Retries added:** `retries`, `retry:`, `--reruns`, `@pytest.mark.flaky`, `this.retries(`.
- **A new test case with no assertion call** in its body (`assert`, `expect(`, `t.Error`, `t.Fatal`, `require.`). Bodies that only check that nothing throws go in an `assert.doesNotThrow`.

Soft hits are not failures. Each becomes a unit for the judged check J1:

- `sleep(`, `setTimeout(`, `time.Sleep(`
- `Date.now()`, `new Date()`, `time.Now()`, `datetime.now`
- `Math.random`, `random.` without a seed
- any URL literal whose host is not `localhost`, `127.0.0.1` or `::1`

Cost: milliseconds.

### G3 `no_new_clones`

- **Rule.** Fail when the delivery adds a clone of at least 50 tokens that the base commit did not have. Clones that already exist never fail a delivery. Test files use 100 tokens because table-driven tests repeat by design.
- **How it runs.** `jscpd --min-tokens 50 --baseline-from-ref <base> --fail-on-new-clones .`. jscpd's README describes the baseline mode as one that "tolerates legacy clones and fails the build on regressions", so no filtering code is needed.
- **Install.** jscpd is installed in the quack-v1 image, not fetched at run time.
- **Cost.** One pass over the repo, seconds at quack's size, with no model.

### Implementer rubric (judged, per item)

This adds to the audit's proposal (gates `claims_match_activity`, `checks_pass`, `sloplint_clean`, `commit_message_form`; model checks `minimal_diff`, `unnamed_files_justified`). The checks need the judge wired on `github.pr_body`, which `post.ts` does not do yet, and EVL-5's 20 cases before they gate a push.

| Check | Weight | scope / chunk / fanIn | Question |
| --- | --- | --- | --- |
| J1 `flake_hit_harmless` | 3 | `flake_hit` (each G2 soft hit, with 15 lines around it) / item / share | Does this test's result stay the same however long the work takes, whatever the date or time, whatever random value is drawn, and whether or not the network is up? |
| J2 `test_can_fail` | 3 | `new_test` (each added test case's body plus the functions it calls in the diff) / item / share | If the code under test returned a wrong value, would an assertion in this test fail? |
| `minimal_diff` | 2 | `hunk` / item / share | Does this hunk serve the task the PR body states? |

## Reviewer

### Prompt lines

Replace the existing test line in "What to check" (item 4, "Tests: would they fail if the code were broken, and do they cover the new branches?") with these three:

```markdown
4. Tests: for each test the diff adds or changes, would it fail if the code were wrong, and can it fail when the code is right? Look for an expected value taken from the test's own stub or from the code under test, a fixed sleep instead of waiting on a condition, a real clock, an unseeded random value, a real network call, or state another test leaves behind. Report either as blocking (tests) and show the input or condition.
5. Duplication: when the diff adds a function, search the repo for one that already does its job, by what it does and not only by its name. Cite it if found.
6. Boundaries: when a public signature, config key or error names the module's storage, transport or vendor, ask whether swapping that dependency would force callers to change. If so, report it with the symbol.
```

This adds about 110 words, and the old items 5 and 6 renumber to 7 and 8. No prompt line covers G-type tokens (`.only`, retries). Those reach the reviewer as evidence.

### Evidence units added to `reviewEvidence`

All are listed in code from the diff, so every check below quantifies over a known set:

- **`new_test`:** each test case the diff adds or changes, found from hunk headers and the test-call patterns of G2, with its body.
- **`flake_hit`:** each G2 soft hit in added test lines, with 15 lines of context.
- **`clone`:** each G3 clone that overlaps added lines, with both fragments.
- **`new_function`:** each added top-level or exported function. The three closest existing functions come from shared camelCase name tokens and jscpd clone pairs.
- **`blocking_tests` and `blocking_design`:** the blocking findings anchored `tests` and `design`, filtered by anchor like today's `blocking` scope.
- For each unit, code records whether any finding cites a line within 3 lines of it (`reported`).

### Rubric checks

Each check is "for each listed item, did the review handle it", split into positive per-item questions. The fan-in rule combines the model's answer with the code's `reported` flag. A unit counts as a miss only when the model says the problem is real and no finding cites it.

`unreported` is a generic fan-in rule, not reviewer code. A unit counts against the check when the model answers yes and the unit's `reported` flag, set by the domain's evidence builder, is false.

| Check | Weight | scope / chunk / fanIn | Question |
| --- | --- | --- | --- |
| `flake_sources_reported` | 3 | `flake_hit` / item / unreported | Does this test's result depend on this line, through a fixed delay where a condition could be awaited, a clock or random value that reaches an assertion, or a network call to a real host? |
| `tautologies_reported` | 3 | `new_test` / item / unreported | Does this test's expected value come from a value the test itself set up (a stub, a mock or a constant), from the code under test, or from the same expression as the actual value, or does no assertion run? |
| `clones_reported` | 2 | `clone` / item / unreported | Do these two fragments implement the same behavior, so that a fix to one would have to be made in the other? |
| `duplicate_helpers_reported` | 2 | `new_function` / pair(new, candidate), fanned out over its 3 candidates / any per function, then unreported | Does the existing function already compute what the new function computes, for the inputs the new function's callers pass? |
| `tests_finding_shows_failure` | 2 | `blocking_tests` (blocking findings with anchor `tests`) / item / share | Does this finding show either a wrong output the test would still pass on, or a condition (timing, order, date, seed, network) under which the test fails on correct code? |
| `leak_finding_substitution` | 1 | `blocking_design` (blocking findings with anchor `design` that name a leak) / item / share | Does this finding name the public symbol that carries the internal name, and would replacing that internal dependency force a caller to change it? |

Every question asks about one item and is phrased so that yes is the finding. None asks whether something is absent from the whole diff.

### Cost

Each unit is one classifier call (Clef) and escalates to the 27B only when uncertain. A typical quack PR has 0-3 flake hits, 1-6 new tests, 0-2 clones and 1-5 new functions (times 3 pairs), so roughly 5-25 small calls of a few hundred tokens each.

- The calls are independent units, so they need fan-out to run in parallel.
- Run serially, they risk the "no slower" bar. Measure before enabling.
- If the 27B escalation rate on a check exceeds its baseline, its weight is set to 0 (observe-only) until fixed.

### The `blocks_without_consequence` conflict

Its question penalizes a blocking finding that rests on "a test that could be stronger". A correct finding that a new test is tautological or flaky also reads as "the test could be stronger" to a classifier, so the pitfall would mark down exactly the findings this proposal asks for.

- Scope `blocks_without_consequence` to blocking findings whose anchor is not `tests`.
- Drop "or a test that could be stronger" from its question.
- Judge `tests` findings by `tests_finding_shows_failure` instead. A weak-assertion nit names no wrong output and no failing condition, so it still loses points, while a tautology or flake finding that shows its failure mode earns them.
- This is a scope change, so the rubric version goes to v6.

## Measurement

### Seeded case set

Open seeded PRs against `fagerbergj/quack`'s `v1` from never-merged branches, or against a scratch repo the owner names. Each PR plants one defect in an otherwise correct change:

| Case | Planted defect | Expected review |
| --- | --- | --- |
| `seed-flaky` | A new `node:test` that starts a timer-driven write, then `await sleep(50)` before asserting | Blocking (tests), citing the sleep |
| `seed-tautology` | A new test that stubs the store to return `{ id: 'x' }` and asserts `id === 'x'` | Blocking (tests), citing stub and assert |
| `seed-leak` | An exported `packages/github` function returns the raw Octokit response type, and `packages/eval` reads an Octokit-only field from it. Import arrows are left legal, so `tools/boundary.ts` stays green | Blocking (design), citing the signature |
| `seed-parallel` | A new `formatDuration()` in `packages/web` beside an existing exported equivalent | Suggestion or blocking, citing the existing one |
| `seed-clean-tests` | A correct change with good new tests that use `setTimeout` inside a polling helper with a timeout | Approve; no flake finding |
| `seed-clean-helper` | A correct new helper whose only similar neighbor has a different, documented policy | Approve; no duplication finding |

### Adding them to `v1-reviewer`

1. Run the reviewer on each seeded PR on the QA (non-prod) server.
2. Add each recorded session: `node packages/eval/src/main.ts dataset v1-reviewer reviewer bad <session-dir>`, with `good` for the two controls.
3. The GitHub eval double serves the pinned PR as open (#2183), so replays review the seeded head.

### Replay

- **Prompt change:** a Langfuse A/B of the full run, in the quack-v1 image, one case at a time, started only while `vllm:num_requests_running` is 2 or lower:

  ```
  docker compose run ... node packages/eval/src/main.ts run --concurrency=1 v1-reviewer <run-name> /config/deployment.json /app/agents/reviewer/agent.json <item-id>
  ```

- **Rubric change:** `node packages/eval/src/replay-judge.ts --arm base=origin/v1 --arm cand=. --repeat 3` on the recorded reviews, per [docs/v1/README.md](../README.md#evidence-for-a-judge-or-rubric-change).
- **Gates G1-G3:** unit tests with fixture diffs, plus one implementer replay where G1 must fail a seeded pass-on-base test.

### The bar

The bar is no slower and no added false positives, starting with one case.

1. **`seed-flaky` alone:** the candidate catches it in 3 of 3 runs; the baseline result is recorded. `seed-clean-tests` is not blocked in 3 of 3. Median grading seconds per case are no higher than baseline.
2. **Widen to all six seeds**, then the full 22-item `v1-reviewer` set:
   - no new false block among the known-good items;
   - no rise in median turns, wall time or escalations against the same-day baseline;
   - each new check's catch counted only over the seeds that target it.
3. **A check that fails the bar** ships at weight 0 (observe) or is cut. It is never rerun until it passes.

## Out

| Indicator | Why it stays out |
| --- | --- |
| "Caller relies on unpromised behavior" (iteration order, internal state) as a rubric check | Highest false-positive class in slop3. Proving it needs the callee's contract, which a diff rarely shows. It stays a reviewer judgement with no score. |
| Naming and convention drift | Judgement only; low harm; the linter decides style (prompt item 8). |
| Narrating comments | Comment runs are already gated at 2 lines by sloplint; paraphrase detection is noisy. |
| Rule-of-three counting, generic DRY scoring | Contested heuristics (slop4); clone and helper checks cover the provable part. |
| Over-engineering beyond `minimal_diff` | Intent-dependent; single-implementation interfaces are sometimes deliberate. |
| Mutation testing per PR | One test run per mutant. Google restricts it to diff-touched covered lines for that reason; too costly in the session sandbox today. |
| Rerunning new tests N times to find flakes | FlakeFlagger: 10 reruns would have found only about a quarter of the flaky tests that 10,000 reruns found, so a green rerun proves little and costs a suite run each time. |
| ML flaky-test prediction, embedding-based clone detection | No local training data; an embedding similarity says two functions look alike, not that they behave alike. |
| Unused exports (whole-program dead code) | Exports used as public API or registered at run time read as unused; `strict` typecheck already covers locals. |
| Snapshot-test policing | Few snapshot tests in quack's repos; revisit if they appear. |

## Open questions for the owner

1. Seeded PRs: on `fagerbergj/quack` from never-merged branches, or on a scratch repo? The first keeps the evidence builder honest about this repo's layout; the second keeps the PR list clean.
2. Should G1-G3 hard-fail `deliver`, or start as text the approver sees, until the implementer has its 20 cases?
3. Is about 110 more prompt words acceptable, against the audit's push to keep the prompt near 850?
4. Should `leak_finding_substitution` ship at all, given that boundary imports are already gated in quack's own CI by `tools/boundary.ts`? It matters only for other repos.
5. Is jscpd acceptable as a new dependency in the quack-v1 image?

## Sources checked

Opened on 2026-11-05 and used as stated:

- **Luo et al., FSE 2014 (paper PDF):** 161 of 201 fixes classified; async wait 74 (45%), concurrency 32 (20%), order dependency 19 (12%); "Most flaky tests (78%) are flaky the first time they are written."
- **FlakeFlagger, ICSE 2021:** of 811 flaky tests found in 10,000 reruns, "only roughly a quarter ... would have been found with 10 reruns, roughly half with 100 reruns and roughly two thirds with 1,000 reruns."
- **Petrović and Ivanković, ICSE-SEIP 2018:** "the reported usefulness of the surfaced results improved from 20% to 80%"; mutants on covered, diff-touched, non-arid lines only.
- **Vera-Pérez et al., EMSE 2019:** pseudo-tested methods in "all 21 mature Java projects" studied, from 1% to 46%.
- **Seemann, "Tautological assertion", 2019:** the quote above.
- **jscpd README and jscpd.dev:** `--baseline`, `--baseline-from-ref`, `--fail-on-new-clones[=N]`, `--changed`, `--threshold`, `--min-tokens`.
- **PMD CPD docs (7.29.0):** `--minimum-tokens` is required; exit 4 on duplication.
- **RepoReuse, arXiv 2609.35357:** agents "leave duplicated logic in 50.8% of task chains by turn 5, all while pass rates barely move."
- **GitClear 2025:** copy/pasted lines rose from 8.3% to 12.3% of changed lines; moved lines fell from 25% to under 10% (2021-2024).
- **Spracklen et al., USENIX Security 2025:** hallucinated packages at least 5.2% (commercial) and 21.7% (open-source); 205,474 unique names.
- **Rules:** Sonar S2925 ("Thread.sleep" should not be used in tests); eslint-plugin-jest `no-focused-tests` and `expect-expect`; `expect.hasAssertions()` in Jest and Vitest.
- **node:test docs (v26.13):** `only` is honored under `--test-only` or with isolation off; `--test-randomize` and `--test-random-seed` exist, at stability 1.0 since v26.1.
- **Wording checked at the source:** Hyrum's Law; Delplanque et al., "Rotten Green Tests" (ICSE 2019).

Corrected from the reports:

- slop1 called FlakeFlagger's rerun curve "~26% / ~45% / ~67%" and "sub-linear". The paper says roughly a quarter, half and two thirds, and does not use "sub-linear".
- slop2 tied the Google 20%-to-80% figure to the current system. It is from the 2018 paper; the 2021 TSE paper reports productive mutants going from 15% to 80%, then 89%.
- slop4 implied `jscpd --changed --fail-on-new-clones`. `--changed` reports on files `git status` lists; the baseline flags are what gate regressions.

Not used because not checked:

- slop1: Google's "16% of tests flaky" and Reservoir; the Microsoft and Firefox figures; DTDetector's 72%; FLAKE16 F1 scores.
- slop2: the 400-PR "moved expectation" replay; goalline and hollowtest results; SpecStory's 80.2% agent-oracle figure.
- slop3: qualirs, ArchLint, candor's "80% to 0%" trial; MageLift's ADR.
- slop4: the stackness.dev and powerduck.com incidents (the `formatCurrency` example is kept as an illustration only); the 617-PR redundancy figure.
- slop5: GitClear's 2026 "+47% error-masking"; the agent-PR study's revert rates; Ultracite's dead-code samples (the "42 of 89 exports" figure); CodeRabbit, CrowdStrike and Ox Security figures.
