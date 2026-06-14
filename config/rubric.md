# Default scoring rubric

This is the default G-Eval scoring guide. It operationalises the global
constitution into named criteria with explicit scoring anchors. Agents that
need domain-specific scoring drop a rubric.md into their bundle directory —
that replaces this file while the constitution remains in effect.

## Evaluation steps

For each criterion: read the definition and scoring anchors, reason about how
the answer performs, then assign a score 0.0–1.0.

---

### `grounded`

Every non-trivial factual claim is supported by a source the agent actually
retrieved (a fetched page or search result). Vague qualifiers like "reportedly"
or "it is known" do not substitute for a retrieved source.

- **1.0** — every non-trivial claim traces to retrieved material
- **0.5** — most claims are sourced; a few minor assertions lack explicit support
- **0.0** — the majority of claims have no retrieved backing

---

### `no_fabrication`

Judge whether anything reads as **invented** — a specific (name, number, price,
date, quote) stated with false confidence that the answer's own evidence and
reasoning don't support. Score on the answer's internal plausibility and
consistency; whether each cited URL is backed by retrieval is checked separately
by deterministic code, so don't second-guess a URL's realness here.

- **1.0** — nothing reads as invented
- **0.5** — minor secondary details look approximate or loosely stated
- **0.0** — a name, number, or quote is clearly fabricated or unsupported by the answer's own evidence

---

### `answers_question`

The response addresses exactly what the user asked, in full — not a
related-but-different question, and not a partial answer that drops part of
the request.

- **1.0** — addresses the request completely
- **0.5** — addresses the main ask; minor gaps
- **0.0** — misses the core ask or redirects to a different question

---

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the
evidence it presents.

- **1.0** — fully consistent throughout
- **0.5** — minor tensions that do not undermine the core conclusion
- **0.0** — clear contradictions, or conclusions the evidence does not support

---

### `cites_sources`

Score whether claims carry followable links at all. Source names alone
("According to Wikipedia…") do not count — a name cannot be checked, only a link
can. For a retrieval agent, the *quality* of those links (whether each URL was
actually fetched/searched) is graded separately by deterministic code and can
override this score; here, judge only the presence of links.

- **1.0** — every non-trivial claim has an inline URL
- **0.5** — URLs for most claims; a few unreferenced
- **0.0** — no URLs cited, only source names with no links

---

### `clean_output`

The response is ONLY the answer — it begins directly with the answer (its title
or first sentence) and ends with the answer (or its `Sources` section). It must
contain no preamble, no process narration, no planning or self-talk, no
meta-commentary about formatting/skills/rules, and no leftover reasoning. The
reader sees the reply verbatim, so anything like "Let me…", "I see, I made a
typo…", "Actually, wait…", "the skill says…", or trailing drafting notes is a
defect — even when the buried content is excellent.

- **1.0** — pure answer; no preamble, narration, or trailing reasoning
- **0.5** — a stray opener or a single meta sentence, otherwise clean
- **0.0** — noticeable preamble and/or leaked planning/reasoning in the output

---

## Zero-retrieval handling

If the agent explicitly states it could not retrieve any sources (tool errors,
no results), score `grounded` and `cites_sources` at **0.0** but do **not**
penalise `answers_question` or `internally_consistent` for the lack of
retrieval — those criteria assess what the agent did with what it had.

## Aggregation rule

Each criterion is an **independent pass/fail** — there is no averaging and no
hard caps. Report `score` as the **lowest** criterion score (the binding
constraint); the gate passes only when every criterion clears the threshold, so
one fatal failure (leaked preamble, no citations) sinks the answer on its own
rather than being averaged away by strong scores elsewhere.

`feedback` must name the lowest-scoring criterion/criteria and what concretely
would fix them so the next revision can act on it.
