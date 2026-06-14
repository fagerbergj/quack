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

## Zero-retrieval handling

If the agent explicitly states it could not retrieve any sources (tool errors,
no results), score `grounded` and `cites_sources` at **0.0** but do **not**
penalise `answers_question` or `internally_consistent` for the lack of
retrieval — those criteria assess what the agent did with what it had.
If the agent silently synthesises without retrieval (no disclaimer), apply the
hard cap as normal.

## Aggregation rule

1. Compute the arithmetic mean of the five criterion scores.
2. **Hard cap**: if `cites_sources` scores **0.0**, the overall score must not
   exceed **0.40**, regardless of the mean. A fluent answer with zero real
   citations is a more dangerous failure than a rough but honest one.
3. Report the capped mean as `score`.

`feedback` must name the specific failing criterion and what concretely would
fix it so the next revision can act on it.
