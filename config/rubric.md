# Default scoring rubric

This is the default G-Eval scoring guide. It operationalises the global constitution into named criteria, each scored on a **0-3 integer scale**. Agents that need domain-specific scoring drop a rubric.yaml into their bundle directory - that replaces this file while the constitution remains in effect.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those steps - cite the specific passage or omission that drives your score.
3. Assign the **integer score (0, 1, 2, or 3)** whose scoring-band descriptor below actually matches the answer.

Score **substance, not style**: length, fluency, and confident phrasing earn no credit on their own.

### The 0-3 scale

Four levels apply to every criterion, and every level has its own written descriptor below - there is no in-between value to guess at. The levels split into two passing and two failing, with no neutral middle to default into:

- **3** - clean: fully met, nothing to note. *Passes.*
- **2** - passes with a noted flaw: met, but with one minor, cosmetic issue that doesn't need to block. *Passes.*
- **1** - deny, small issues: a material gap that needs a touch-up before this clears. *Fails.*
- **0** - deny, major issues: the central requirement is unmet. *Fails.*

---

### `grounded`

Every non-trivial factual claim traces to a source the agent actually retrieved this session (a fetched page or search result), and nothing reads as invented - no specific (name, number, price, date, quote) is stated with more confidence than the answer's own evidence supports. Vague qualifiers like "reportedly" or "it is known" do not substitute for a retrieved source.

You cannot see the agent's retrieval log, and your own knowledge may be stale or incomplete. Judge grounding by whether each claim **carries an inline citation**; do **NOT** lower this score because a cited fact is unfamiliar or recent, and do **NOT** flag a specific as fabricated merely because it postdates your training. A specific is "invented" only when the answer's own text is internally inconsistent or makes a precise claim it never supports - never because it conflicts with your memory.

**Evaluation steps.**
1. List the answer's non-trivial factual claims and specifics.
2. For each, check whether it carries an inline citation, not whether you personally believe it.
3. For each, judge whether the answer's own evidence justifies the confidence it is stated with.

**Scoring bands.**
- **3** - essentially every non-trivial claim traces to retrieved material, and nothing reads as invented.
- **2** - essentially every claim is traceable and nothing reads as invented, but one minor secondary detail is stated a touch more confidently than its source without being unsupported.
- **1** - most claims are sourced or plausible; a few lack explicit support, or minor secondary details look loosely stated.
- **0** - the majority of claims have no retrieved backing, or a name, number, or quote is clearly fabricated or unsupported by the answer's own evidence.

---

### `answers_question`

The response addresses exactly what the user asked, in full - not a related-but-different question, and not a partial answer that drops part of the request.

**Evaluation steps.**
1. Decompose the question into its distinct asks and constraints.
2. Check that each is addressed.
3. Note any silent narrowing or topic drift.

**Scoring bands.**
- **3** - addresses the request completely.
- **2** - addresses the request completely, but one part is covered more thinly than the rest without being dropped.
- **1** - addresses the main ask; minor gaps.
- **0** - misses the core ask or redirects to a different question.

---

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the evidence it presents.

**Evaluation steps.**
1. Check for self-contradiction across the answer.
2. Check that conclusions follow from the cited evidence.
3. Check that uncertainty is stated where evidence is thin.

**Scoring bands.**
- **3** - fully consistent throughout.
- **2** - fully consistent, but one hedge in the answer's own phrasing is a little uneven without an actual contradiction.
- **1** - minor tensions that do not undermine the core conclusion.
- **0** - clear contradictions, or conclusions the evidence does not support.

---

### `cites_sources`

A citation-existence check (does a followable link/path back this claim at all) runs separately as a deterministic check and can override this score. Here, judge citation **quality and placement**: is the cited source reputable for the claim it backs, is it cited accurately (the source actually says what the claim attributes to it), and does the citation sit next to the claim it supports rather than buried in an unrelated list.

**Evaluation steps.**
1. For each cited claim, check the source is a reasonable authority for that claim, not a tangential or low-quality page.
2. Check the claim accurately reflects what the source says.
3. Check the citation is placed at the claim, not deferred to a block the claim isn't attached to.

**Scoring bands.**
- **3** - cited claims point at reputable sources, accurately reflect them, and are placed at the claim they support.
- **2** - sources are reputable and accurate, but one citation sits a sentence or two away from the claim it backs rather than directly at it.
- **1** - one citation is a weak/tangential source, is loosely paraphrased, or is placed away from the claim it backs.
- **0** - a citation misrepresents its source, or citations are only deferred to a references block with no inline placement.

---

### `clean_output`

The answer is formatted for the reader it is addressed to: scannable, and its structure matches its content. It begins directly with the answer (its title or first sentence) and ends with the answer (or its `Sources` section) - no preamble, no process narration, no meta-commentary about formatting/skills/rules. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion - a code snippet, a list, a decision - written out more than once on the way to a final version. The reader sees the reply verbatim, so anything like "Let me…", "I see, I made a typo…", "Actually, wait…", "the skill says…", or trailing drafting notes is a defect - even when the buried content is excellent.

**Evaluation steps.**
1. Read the first sentence: direct answer, or preamble / process narration?
2. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
3. Check whether any snippet, list, or conclusion appears more than once in different (superseded) forms - a sign of an abandoned draft left in place.
4. Check the structure (headings, lists, code blocks) matches what the content actually needs, rather than being flat prose or over-decorated.

**Scoring bands.**
- **3** - pure answer, well-structured for its content; no preamble, narration, or trailing reasoning; only the final version of any content appears.
- **2** - a stray opener or a single meta sentence, or structure that doesn't quite fit the content, otherwise clean - one small blemish, not a pattern.
- **1** - more than one such intrusion (several stray sentences or meta comments), though still short of a real preamble or leaked reasoning.
- **0** - noticeable preamble, leaked planning/reasoning, or a duplicated/superseded draft left in the output.

---

## Zero-retrieval handling

If the agent explicitly states it could not retrieve any sources (tool errors, no results), score `grounded` and `cites_sources` at **0** but do **not** penalise `answers_question` or `internally_consistent` for the lack of retrieval - those criteria assess what the agent did with what it had.

## Aggregation

Each criterion is an **independent requirement**, scored 0-3 and normalised to 0.0-1.0 (divide by 3). The overall score is the **lowest** criterion - the binding constraint (weakest-link gating). There is **no averaging and no caps**: one fatal failure (leaked preamble, no citations, fabricated specifics) sinks the answer on its own rather than being averaged away by strong scores elsewhere. The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them so the next revision can act on it.
