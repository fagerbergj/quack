# Severity: what blocks, what doesn't

The dividing line is not how strongly you feel. It is whether you can point at something objective.

> Technical facts and data overrule opinions and personal preferences.
> — Google, *The Standard of Code Review*

A finding supported only by your preference is a `nit:`, however sure you are. A finding supported by an anchor below is `blocking:`, however small it looks.

## The anchors

Calibrate before you reach for one. Google's standard is that a reviewer approves once the change **definitely improves overall code health**, even where it is not perfect - so on a well-made change the expected outcome is approve with comments, and a blocking finding is the exception you have to earn. Measured on ten real merged pull requests, this skill blocked all ten, including the five their own reviewer approved: the anchors below were wide enough that something always qualified.

`blocking:` requires ONE of these, and the finding must say which:

| Anchor | What it means | Not this |
|---|---|---|
| **defect** | Concrete inputs or state produce wrong output, a crash, or data loss | "This feels fragile" |
| **security** | Auth flaw, unvalidated input, injection, PII or secret exposure | "I'd sanitise this differently" |
| **design** | A *named* principle violated, **and** you can say which concrete future defect it invites | Naming a principle you could name about most code |
| **scope** | The change does something its task never asked for | A pre-existing wart in adjacent code |
| **tests** | The behaviour this change *exists to deliver* ships with no test, or a test passes while what it claims to cover is broken | An untested edge case, fallback, or guard alongside a tested main path - that is a `suggestion:` |

There is deliberately no *intent* anchor.
A human reviewer blocking on "I don't understand this" is healthy scepticism; an automated reviewer doing it would block on anything unfamiliar, at any hour, with nobody to argue back.
See **Unclear intent** below for where it goes instead.

`suggestion:` — a better approach exists and you say **why**, citing a principle or a measurement.
The author may decline and merge anyway.

`nit:` — style or preference **not** governed by the project's style guide or linter.
The author may ignore it. Never blocks.

`question:` — you suspect a problem but aren't sure.
Asking resolves it faster than demanding a change.
A question does **not** withhold approval on its own: an automated reviewer verifying rather than trusting will always have one, so letting questions gate approval means never approving. Withhold only when the answer would land on a defect anchor above, and say so.

`praise:` — summary only, never inline.

## Hard rules

**The style guide and the linter are the authority on style.**
A point the linter already enforces is the linter's job, not a finding.
Where no convention exists, accept the author's choice rather than inventing one.

**Design is not style.**
"Aspects of software design are almost never a pure style issue or just a personal preference. They are based on underlying principles and should be weighed on those principles, not simply by personal opinion" (Google).
Naming the principle is what separates a design block from a taste argument - and naming the concrete future defect it invites is what separates a block from a principle you could cite about most code.

**Out of scope is not blocking here.**
Concerns about adjacent code the change doesn't touch belong in a separate issue.
Raise them, file them, don't hold this change hostage to them.

**Forward progress.**
Approve a change that clearly improves overall code health even if it isn't perfect.
There is no perfect code, only better code.
Blocking a net improvement on polish costs more than the polish is worth.

**Unclear intent withholds approval; it does not demand changes.**
If you cannot verify correctness because you don't understand the change, raise a `question:` naming *what specifically* you could not determine, and let the verdict be `comment`.

You are working with a bounded context.
Not understanding something is at least as likely to mean you are missing a file, a convention, or a prior decision as it is to mean the code is wrong — so it is not grounds to demand the author change anything.
It *is* grounds to decline to sign off: an approval you cannot stand behind is worth nothing.

This departs deliberately from the human guidance, which escalates can't-understand to request-changes on the reasoning that a confused reviewer predicts a confused future reader.
That assumes a reviewer who has read everything and will be argued with. Neither holds here.

**Never assert behaviour about code you didn't open.**
A finding about code you never read is fabrication, whatever label you put on it.

## The verdict follows mechanically

Three verdicts, three levels of confidence:

- Any surviving `blocking:` → **request changes**. You found a defect.
- Else any unresolved `question:` **whose answer could land on an anchor above** → **comment**. You may be missing something; approval withheld, no changes demanded. A question that could not change the verdict either way does not reach this rung - see `question:` above, and note that a reviewer told to verify rather than trust always has one.
- Else → **approve**, even with suggestions and nits outstanding; say explicitly that they're non-blocking.

### request changes

At least one issue must be resolved before merge, and the summary **names which one**, so the author doesn't have to guess which of your comments is the gate.

The threshold is Google's: nothing justifies merging a change that *definitely worsens* the overall code health of the system.
Anything short of that is a comment, not a gate.

The anti-pattern, stated plainly by Konishi: requesting changes to express taste rather than identify a defect.
If the issue is taste, mark it a nit and approve.

### approve

You have nothing that requires the author to act.
Not "the change is perfect" — Google: favour approving once it *definitely improves* code health, even if it isn't perfect.
GitLab frames the same test as net profit: weigh the value of the change and of future maintainability against the cost of holding it.

Approving with non-blocking comments attached is a normal outcome, not a compromise.
Say the comments are non-blocking so the author knows they can merge.

### comment

Reserved. Use it when you have neither a block nor a green light — verification you could not finish, or a gap you need a human to weigh.
State what you couldn't verify.

Industry guidance treats `comment` as the ordinary vehicle for non-blocking feedback, on the assumption a human merges anyway.
**That assumption does not hold here.** Where an automated merge path requires an explicit approval, a `comment` verdict silently withholds it — it reads as neutral and behaves as a soft block, with no stated reason anyone can act on.
So: approve with non-blocking comments, and keep `comment` for the case it's named for.

A `comment` verdict with no blocking findings and no stated gap carries no signal: it neither stops a merge nor endorses one.

## Worked calls

**Blocks.** `drawCard` does `copy.shift()!` with no empty-deck guard.
A non-null assertion is not a check, and once the deck is exhausted this throws.
Anchor: defect, with the inputs stated.

**Blocks.** `dealerDrawRule` is exported and tested, but the game inlines its own copy and never calls it.
The suite passes while the played rule drifts from the tested rule, silently.
Anchor: tests.

**Blocks.** A handler merges results from two stores; no test configures both.
Anchor: tests — the failure mode is "this breaks and nothing tells you".

**Does not block.** Two constants both default to 50 and could drift.
Real, worth fixing, no scenario where it misbehaves today. `suggestion:`.

**Does not block.** `startsWith('/memory')` also matches `/memory-export`.
With two routes today nothing breaks, but say so — it's a latent defect rather than a taste call. `suggestion:`.

**Nit.** Import ordering the linter doesn't enforce.

## Sources

- [The Standard of Code Review](https://google.github.io/eng-practices/review/reviewer/standard.html) — Google. Facts over preferences; style guide as authority; design is not style; approve on net improvement.
- [Reviewer Guidance](https://microsoft.github.io/code-with-engineering-playbook/code-reviews/process-guidance/reviewer-guidance/) — Microsoft. Business-logic and test correctness as the reviewer's focus; tests ship in the same PR; scope discipline.
- [Code Review Values](https://handbook.gitlab.com/handbook/engineering/workflow/reviewer-values/) — GitLab. Blocking vs non-blocking taxonomy.
- [Conventional Comments](https://conventionalcomments.org/) — the label vocabulary; `nitpick` as non-blocking by nature.
- [How we review PRs](https://posthog.com/handbook/engineering/how-we-review) — PostHog. Approve / Comment / Request Changes semantics.
- [Use prefixes to improve code review communication](https://www.ssw.com.au/rules/use-prefixes-to-improve-code-review-communication) — SSW. Prefix-to-blocking mapping.
- [Code Review Checklist and Anti-Pattern Catalog](https://hidekazu-konishi.com/entry/code_review_checklist_and_antipatterns.html) — Konishi. Name the specific blocker; request-changes-as-taste is an anti-pattern; approve-with-comments as the middle ground.
- [Code Review Guidelines](https://docs.gitlab.com/development/code_review/) — GitLab. Mark non-mandatory suggestions non-blocking and move forward rather than waiting.
- [When (and When Not) to Use "Request Changes"](https://gustavoaraujo.dev/code-review-best-practices-when-and-when-not-to-use-request-changes) — build breakage, security, and architectural violations as the blocking set.
