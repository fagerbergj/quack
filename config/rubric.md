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

Specific names (businesses, operators, products, people), numbers (prices,
speeds, distances, temperatures, ratings), dates, quotes, and URLs must all
appear in the retrieved material. Unverified specifics are never invented.

**Any quantitative claim — a percentage, count, threshold, benchmark score, or
time value — must be traceable to a retrieved source.** A plausible-sounding
number stated without a source is fabrication, not approximation.

Specific business names, operator details, and statistics are **high-risk
fabrication vectors**: treat unverified specifics here as a near-fail.

- **1.0** — all specifics and quantitative claims are verified in retrieved material
- **0.5** — minor or secondary details are approximate; no names, prices, or quantitative claims are fabricated
- **0.0** — names, prices, statistics, or any specific number appears without a retrieved source

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

Every non-trivial claim has a URL the reader can follow to verify it. Source
names alone ("According to Wikipedia…") do not count — a name cannot be
checked, only a link can.

- **1.0** — URLs provided for all non-trivial claims
- **0.5** — URLs for most claims; a few unreferenced
- **0.0** — no URLs cited, or only source names with no links

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
   exceed **0.40**, regardless of the mean. A fluent, well-structured answer
   with zero citations is a more dangerous failure than a rough but honest one.
3. Report the capped mean as `score`.

`feedback` must name the specific failing criterion and what concretely would
fix it so the next revision can act on it.
