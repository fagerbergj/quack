# Default scoring rubric

This is the default G-Eval scoring guide. It operationalises the global constitution into named criteria, each scored on a **0-3 integer scale**. Agents that need domain-specific scoring drop a rubric.yaml into their bundle directory - that replaces this file while the constitution remains in effect.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those steps - cite the specific passage or omission that drives your score.
3. Assign the **integer score (0, 1, 2, or 3)** whose scoring-band descriptor below actually matches the answer.

Score **substance, not style**: length, fluency, and confident phrasing earn no credit on their own.

### The 0-3 scale

Every criterion is scored by counting findings, never by weighing adjectives. A finding is one concrete failure of the criterion, recorded with a verbatim quote from the answer (or an omission naming exactly what is missing); a finding you cannot quote or name does not count. Enumerate the items the criterion covers first, apply its steps to each, then pick the band from the count:

- **3** - no finding. *Passes.* Do not search for a blemish to justify a 2: none found is the top band.
- **2** - exactly one finding, and it is minor as the criterion defines minor. *Passes.*
- **1** - two or three findings, or one finding on a load-bearing item as the criterion defines it. *Fails.*
- **0** - four or more findings, or the central failure the criterion names. *Fails.*

---

### `grounded`

Every non-trivial factual claim traces to a source the agent actually retrieved this session (a fetched page or search result), and nothing reads as invented - no specific (name, number, price, date, quote) is stated with more confidence than the answer's own evidence supports. Vague qualifiers like "reportedly" or "it is known" do not substitute for a retrieved source.

You cannot see the agent's retrieval log, and your own knowledge may be stale or incomplete. Judge grounding by whether each claim **carries an inline citation**; do **NOT** lower this score because a cited fact is unfamiliar or recent, and do **NOT** flag a specific as fabricated merely because it postdates your training. A specific is "invented" only when the answer's own text is internally inconsistent or makes a precise claim it never supports - never because it conflicts with your memory.

**Evaluation steps.**
1. Enumerate first: list every item the steps below cover, then apply them to each one. A finding is a non-trivial claim or specific (name, number, price, date, quote) with no inline citation, or one stated more confidently than the answer's own evidence supports. Record each finding with a verbatim quote (or an omission naming exactly what is missing); one you cannot quote or name does not count. Pick the band from the number of findings; none is the top band.
2. List the answer's non-trivial factual claims and specifics.
3. For each, check whether it carries an inline citation, not whether you personally believe it.
4. For each, judge whether the answer's own evidence justifies the confidence it is stated with.

**Scoring bands.**
- **3** - no finding: every item passes the steps above.
- **2** - exactly one finding, and it is a secondary detail (not a headline figure or a conclusion) and it is cited, only overstated.
- **1** - two or three findings, or one finding that is a headline figure, a conclusion, or a specific with no citation at all.
- **0** - four or more findings, or a name, number, or quote is contradicted by the answer's own evidence, or the majority of claims carry no citation.

---

### `answers_question`

The response addresses exactly what the user asked, in full - not a related-but-different question, and not a partial answer that drops part of the request.

**Evaluation steps.**
1. Enumerate first: list every item the steps below cover, then apply them to each one. A finding is a distinct ask or constraint in the request that the answer does not address. Record each finding with a verbatim quote (or an omission naming exactly what is missing); one you cannot quote or name does not count. Pick the band from the number of findings; none is the top band.
2. Decompose the question into its distinct asks and constraints.
3. Check that each is addressed.
4. Note any silent narrowing or topic drift.

**Scoring bands.**
- **3** - no finding: every item passes the steps above.
- **2** - exactly one finding, and the ask is addressed, only more thinly than the others.
- **1** - two or three findings, or one finding that drops or silently narrows an ask or constraint.
- **0** - four or more findings, or the core ask is missed, or the answer addresses a different question.

---

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the evidence it presents.

**Evaluation steps.**
1. Enumerate first: list every item the steps below cover, then apply them to each one. A finding is a place where the answer contradicts itself, a conclusion its own evidence does not support, or a hedge that later becomes a certainty. Record each finding with a verbatim quote (or an omission naming exactly what is missing); one you cannot quote or name does not count. Pick the band from the number of findings; none is the top band.
2. Check for self-contradiction across the answer.
3. Check that conclusions follow from the cited evidence.
4. Check that uncertainty is stated where evidence is thin.

**Scoring bands.**
- **3** - no finding: every item passes the steps above.
- **2** - exactly one finding, and it is a hedge phrased unevenly with no actual contradiction.
- **1** - two or three findings, or one finding that is a contradiction between two claims, or a conclusion the cited evidence does not support.
- **0** - four or more findings, or the main conclusion contradicts the answer's own evidence.

---

### `cites_sources`

Code-owned: deterministic code scores whether each cited link was fetched or seen this session (see the bands in every research rubric). The judge does not score this criterion; citation quality and placement are `citation_quality` where a rubric declares it.

---

### `clean_output`

The answer is formatted for the reader it is addressed to: scannable, and its structure matches its content. It begins directly with the answer (its title or first sentence) and ends with the answer (or its `Sources` section) - no preamble, no process narration, no meta-commentary about formatting/skills/rules. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion - a code snippet, a list, a decision - written out more than once on the way to a final version. The reader sees the reply verbatim, so anything like "Let me…", "I see, I made a typo…", "Actually, wait…", "the skill says…", or trailing drafting notes is a defect - even when the buried content is excellent.

**Evaluation steps.**
1. Enumerate first: list every item the steps below cover, then apply them to each one. A finding is a sentence or block that is not the answer: preamble, process narration, self-talk, meta-commentary about skills or formatting, leaked reasoning, or a superseded draft left in place. Record each finding with a verbatim quote (or an omission naming exactly what is missing); one you cannot quote or name does not count. Pick the band from the number of findings; none is the top band.
2. Read the first sentence: direct answer, or preamble / process narration?
3. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
4. Check whether any snippet, list, or conclusion appears more than once in different (superseded) forms.
5. Check the structure (headings, lists, code blocks) matches what the content actually needs.

**Scoring bands.**
- **3** - no finding: every item passes the steps above.
- **2** - exactly one finding, and it is one stray sentence (an opener or a meta remark) with the rest clean.
- **1** - two or three findings, or one finding that is a paragraph of narration or reasoning, or a duplicated section.
- **0** - four or more findings, or the answer opens with a preamble or planning, or a superseded draft sits beside the final one.

---

## Zero-retrieval handling

If the agent explicitly states it could not retrieve any sources (tool errors, no results), score `grounded` and `cites_sources` at **0** but do **not** penalise `answers_question` or `internally_consistent` for the lack of retrieval - those criteria assess what the agent did with what it had.

## Aggregation

Each criterion is an **independent requirement**, scored 0-3 and normalised to 0.0-1.0 (divide by 3). The overall score is the **lowest** criterion - the binding constraint (weakest-link gating). There is **no averaging and no caps**: one fatal failure (leaked preamble, no citations, fabricated specifics) sinks the answer on its own rather than being averaged away by strong scores elsewhere. The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them so the next revision can act on it.
