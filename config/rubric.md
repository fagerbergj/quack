# Default scoring rubric

This is the default G-Eval scoring guide. It operationalises the global
constitution into named criteria, each scored on a **0–10 integer scale**.
Agents that need domain-specific scoring drop a rubric.md into their bundle
directory - that replaces this file while the constitution remains in effect.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those
   steps - cite the specific passage or omission that drives your score.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's
   own scoring bands.

Score **substance, not style**: length, fluency, and confident phrasing earn no
credit on their own.

### The 0–10 scale

The same scale applies to every criterion. The per-criterion **scoring bands**
below tell you what "met", "partially met", and "failed" mean *for that
criterion*; this scale tells you which number within those ranges to pick.

- **10** - flawless on this criterion; nothing to fault.
- **9** - met; only a trivial, cosmetic nitpick.
- **8** - met; one minor gap that does not weaken the answer.
- **7** - met, but barely; a real (if small) shortcoming. *Lowest passing score -
  the gate's threshold sits here.*
- **6** - partially met; a noticeable weakness that should be fixed.
- **5** - partially met; as much wrong as right.
- **4** - mostly unmet; more violated than satisfied.
- **3** - largely failed; a serious problem on this dimension.
- **2** - failed; essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or what this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met
more completely or the flaw is more trivial; the lower number when it only just
clears the band. Do not default to 0, 5, or 10.

---

### `grounded`

Every non-trivial factual claim is supported by a source the agent actually
retrieved this session (a fetched page or search result). Vague qualifiers like
"reportedly" or "it is known" do not substitute for a retrieved source.

You cannot see the agent's retrieval log, and your own knowledge may be stale or
incomplete. Judge grounding by whether each claim **carries an inline citation**;
do **NOT** lower this score because a cited fact is unfamiliar or recent.

**Evaluation steps.**
1. List the answer's non-trivial factual claims.
2. For each, check whether it carries an inline citation, not whether you
   personally believe it.
3. Score the proportion that carry a citation.

**Scoring bands.**
- **7–10** - essentially every non-trivial claim traces to retrieved material.
- **4–6** - most claims are sourced; several lack explicit support.
- **0–3** - the majority of claims have no retrieved backing.

---

### `no_fabrication`

Judge whether anything reads as **invented** - a specific (name, number, price,
date, quote) stated with false confidence that the answer's own evidence and
reasoning do not support. Score on internal plausibility and consistency;
whether each cited URL is backed by retrieval is checked separately by
deterministic code, so do not second-guess a URL's realness here.

**Recency caveat - critical.** The agent retrieved live content you do not have,
and your own knowledge is stale and incomplete. Do **NOT** flag a claim as
fabricated merely because you don't recognize it, it sounds new, or it postdates
your training. A specific is "invented" only when the answer's own text is
internally inconsistent or makes a precise claim it never supports - never
because it conflicts with your memory.

**Evaluation steps.**
1. Identify every specific named or quantitative claim.
2. For each, judge whether the answer's own evidence justifies its confidence.
3. Weight load-bearing specifics most heavily.

**Scoring bands.**
- **7–10** - nothing reads as invented.
- **4–6** - minor secondary details look approximate or loosely stated.
- **0–3** - a name, number, or quote is clearly fabricated or unsupported by the
  answer's own evidence.

---

### `answers_question`

The response addresses exactly what the user asked, in full - not a
related-but-different question, and not a partial answer that drops part of the
request.

**Evaluation steps.**
1. Decompose the question into its distinct asks and constraints.
2. Check that each is addressed.
3. Note any silent narrowing or topic drift.

**Scoring bands.**
- **7–10** - addresses the request completely.
- **4–6** - addresses the main ask; minor gaps.
- **0–3** - misses the core ask or redirects to a different question.

---

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the
evidence it presents.

**Evaluation steps.**
1. Check for self-contradiction across the answer.
2. Check that conclusions follow from the cited evidence.
3. Check that uncertainty is stated where evidence is thin.

**Scoring bands.**
- **7–10** - fully consistent throughout.
- **4–6** - minor tensions that do not undermine the core conclusion.
- **0–3** - clear contradictions, or conclusions the evidence does not support.

---

### `cites_sources`

Score whether claims carry followable links at all. Source names alone
("According to Wikipedia…") do not count - a name cannot be checked, only a link
can. For a retrieval agent, the *quality* of those links (whether each URL was
actually fetched/searched) is graded separately by deterministic code and can
override this score; here, judge only the presence of links.

**Evaluation steps.**
1. Count the answer's non-trivial claims.
2. Count how many carry an inline, followable URL.
3. Score the proportion that are linked.

**Scoring bands.**
- **7–10** - every non-trivial claim has an inline URL.
- **4–6** - URLs for most claims; a few unreferenced.
- **0–3** - no URLs cited, only source names with no links.

---

### `clean_output`

The response is ONLY the answer - it begins directly with the answer (its title
or first sentence) and ends with the answer (or its `Sources` section). It must
contain no preamble, no process narration, no planning or self-talk, no
meta-commentary about formatting/skills/rules, and no leftover reasoning. This
includes mid-body deliberation: visible self-correction ("Actually, let me
reconsider…"), an abandoned or superseded draft left in place, or the same
conclusion - a code snippet, a list, a decision - written out more than once on
the way to a final version. The reader sees the reply verbatim, so anything like
"Let me…", "I see, I made a typo…", "Actually, wait…", "the skill says…", or
trailing drafting notes is a defect - even when the buried content is excellent.

**Evaluation steps.**
1. Read the first sentence: direct answer, or preamble / process narration?
2. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
3. Check whether any snippet, list, or conclusion appears more than once in
   different (superseded) forms - a sign of an abandoned draft left in place.
4. Score down for each intrusion; the buried answer's quality does not excuse it.

**Scoring bands.**
- **7–10** - pure answer; no preamble, narration, or trailing reasoning; only
  the final version of any content appears.
- **4–6** - a stray opener or a single meta sentence, otherwise clean.
- **0–3** - noticeable preamble, leaked planning/reasoning, or a
  duplicated/superseded draft left in the output.

---

## Zero-retrieval handling

If the agent explicitly states it could not retrieve any sources (tool errors,
no results), score `grounded` and `cites_sources` at **0** but do **not**
penalise `answers_question` or `internally_consistent` for the lack of
retrieval - those criteria assess what the agent did with what it had.

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to
0.0–1.0 (divide by 10). The overall score is the **lowest** criterion - the
binding constraint (weakest-link gating). There is **no averaging and no caps**:
one fatal failure (leaked preamble, no citations, fabricated specifics) sinks
the answer on its own rather than being averaged away by strong scores
elsewhere. The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely
would fix them so the next revision can act on it.
