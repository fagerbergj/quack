# Code-explorer scoring rubric

This overrides the default rubric (`config/rubric.md`) for `code-explorer`. It
grades an **exploration**, not a piece of web research: the explorer's job is to
read a codebase and report an accurate, useful understanding of it, and its
"sources" are the FILES it actually read — not web pages. So this rubric grades
code-grounding, accuracy, coverage, and usefulness to a downstream implementer.
It deliberately does **not** carry the web-researcher rubric's web-URL
`cites_sources` or web-retrieval `grounded_in_retrieval` criteria — a codebase
explorer that never touches the web is doing exactly its job, and must not be
penalised for the absence of web citations.

Score the **understanding the answer conveys**, grounded in what the worker
actually read. When you have read-only workspace tools (read_file, list_dir,
glob, grep) and a clone is available, OPEN a few of the files the answer
describes and check the description against what they actually contain — do not
take the answer's word for how the code works. Keep the bar FAIR: this is
exploration, not exhaustive documentation. A tight, accurate answer to a
narrow question ("where is X implemented?") should score high near-trivially;
depth is expected only in proportion to what was asked.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those
   steps — cite the specific claim, file reference, or omission that drives
   your score, not a general impression.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's
   own scoring bands.

Score **substance, not style**: a long, fluent, confident-sounding walk-through
is not automatically better than a short, plain, accurate one. Length and polish
earn no credit.

### The 0–10 scale

The same scale applies to every criterion. The per-criterion **scoring bands**
below tell you what "met", "partially met", and "failed" mean *for that
criterion*; this scale tells you which number within those ranges to pick.

- **10** — flawless on this criterion; you can find nothing to fault.
- **9** — met; only a trivial, cosmetic nitpick.
- **8** — met; one minor gap that a careful reader would notice but that does
  not weaken the answer.
- **7** — met, but barely; a real (if small) shortcoming. *This is the lowest
  passing score — the gate's threshold sits here.*
- **6** — partially met; a noticeable weakness that should be fixed before this
  is acceptable.
- **5** — partially met; the weakness is squarely in the middle — as much wrong
  as right.
- **4** — mostly unmet; the criterion is more violated than satisfied.
- **3** — largely failed; a serious problem on this dimension.
- **2** — failed; the criterion is essentially not satisfied.
- **1** — failed badly; actively wrong on this dimension.
- **0** — total failure, or the thing this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met
more completely or the flaw is more trivial; the lower number when it only just
clears the band. Do not default to 0, 5, or 10 — if the answer sits between two
levels, choose the one whose description fits the dominant impression.

**The workspace activity ledger.** Your prompt contains a "Workspace activity"
section: the fs/git operations the worker ACTUALLY performed, reconstructed from
its session by code — not from its narration. It is ground truth. A file the
answer claims to describe that has no `read_file` entry was not read, no matter
how confidently it is described; `read_file` entries carry a content sample —
use it to spot-check quoted or paraphrased file content.

---

### `grounded_in_code`

Every claim the answer makes about the code — what a function does, how a
convention works, that module A calls module B, where X lives — traces to a file
the worker actually read this session (a `read_file`, or a file under a
`git_clone`'d repo it explored). This is the code-grounding criterion; it
replaces the web-researcher's URL-citation check. General knowledge, prior
training, or inference from a filename does **not** count as grounding — the
whole point of an explorer is that it *looked*.

Judge grounding by whether each non-trivial claim is backed by a file in the
ledger and, where the answer cites `<repo>@<path>`, whether that path was
actually read. Do **NOT** lower this score merely because a described API or
pattern is unfamiliar to you — an explorer reads code you cannot see. Lower it
when a claim about the code has no read behind it (an assertion about a file the
ledger shows was never opened) or when a quote/paraphrase contradicts the read
sample.

**Evaluation steps.**
1. List the answer's non-trivial claims about the code.
2. For each, check whether a corresponding file appears in the ledger (read, or
   under a cloned repo) — and, if the answer quotes or paraphrases file content,
   whether the read sample is consistent with it.
3. Treat a claim about a file the ledger never recorded as read as ungrounded;
   treat an honest "I did not read/verify this" as a disclosure, not a
   fabrication.
4. Score the proportion of claims that are backed by actual reads.

**Scoring bands.**
- **7–10** — essentially every claim about the code traces to a file the worker
  read; quotes match their read samples.
- **4–6** — most claims are grounded, but several rest on inference from names
  or on files never opened, or the answer over-quotes beyond what it read.
- **0–3** — the answer asserts how the code works largely from guesswork —
  describing files the ledger shows were never read — or quotes content its
  reads cannot support.

---

### `accurate`

The understanding matches what the code actually does. No misrepresentation, no
plausible-but-wrong description, no guessing dressed as fact. This is the
correctness of the exploration: a confident description that gets the behavior,
control flow, or structure wrong actively misleads a downstream implementer and
is worse than a hedged one.

When you can, open a file the answer describes and compare. Weight the load-
bearing claims — the ones an implementer would act on — most heavily.

**Evaluation steps.**
1. Identify the answer's factual claims about behavior, structure, and
   conventions.
2. For the load-bearing ones, check them against the actual code (via your
   read-only tools where a clone is available, or against the ledger's read
   samples).
3. Judge whether uncertainty is stated where the code was ambiguous, rather than
   papered over with false confidence.

**Scoring bands.**
- **7–10** — the description matches the code; any uncertainty is flagged
  honestly.
- **4–6** — mostly accurate, but a secondary claim is wrong or misleading, or a
  guess is stated as fact without hedging.
- **0–3** — a core claim about how the code works is wrong, or the answer
  confidently misrepresents the structure/behavior.

---

### `answers_the_task`

The response covers what was actually asked — the specific structure, the named
convention, how the requested feature works — at a depth useful for the task,
not a related-but-different tour of the repo.

**Evaluation steps.**
1. Decompose the request into its distinct asks (the specific thing to explain,
   any named files/areas/constraints).
2. Check that each ask is addressed from the actual code.
3. Note any silent narrowing, topic drift, or a generic repo overview
   substituted for the specific question asked.

**Scoring bands.**
- **7–10** — addresses the request completely, at a depth proportionate to what
  was asked.
- **4–6** — addresses the main ask but leaves a sub-question thin or drops a
  named area.
- **0–3** — misses the core ask, or answers a generic "what is this repo" in
  place of the specific question.

---

### `clear_and_actionable`

The understanding is organized and precise enough for a downstream implementer
to act on: exact file/path/symbol references (not vague gestures like "somewhere
in the server layer"), a structure that leads the reader through the answer, and
enough specificity that the reader knows which files they'd touch and which
patterns they'd follow.

**Evaluation steps.**
1. Check that key claims name specific files/paths/symbols rather than gesturing
   at areas.
2. Judge whether the organization (sections, ordering) makes the answer easy to
   act on, or whether it is an undifferentiated wall.
3. Confirm the response is ONLY the answer — it begins directly (no "Great, I
   explored the repo" preamble) and ends with the answer, with no leaked
   planning, self-talk, or meta-commentary about tools/skills. Score down for
   each such intrusion; the buried content's quality does not excuse it.

**Scoring bands.**
- **7–10** — precise file/symbol references, well-organized, directly
  actionable; pure answer with no preamble or leaked reasoning.
- **4–6** — useful but vague in places (an area named where a file was needed),
  or a stray opener/meta sentence mars an otherwise clean answer.
- **0–3** — too vague to act on (no concrete references), disorganized, or
  noticeably marred by preamble/leaked planning.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to
0.0–1.0 (divide by 10). The overall score is the **lowest** criterion — the
binding constraint (weakest-link gating). There is **no averaging and no caps**:
a single failing criterion sinks the answer on its own rather than being
averaged away by strong scores elsewhere, and a strong dimension never excuses a
weak one. The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely
would fix them so the next revision can act on it.
