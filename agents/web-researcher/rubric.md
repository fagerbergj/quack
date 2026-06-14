# Web researcher rubric

The web researcher retrieves and synthesises live web content. It has access to
`current_date`, `web_search`, `web_fetch`, and `summarize` tools. The
independent judge evaluates each answer by scoring the five criteria below
explicitly, in order, before computing an overall score.

## Evaluation steps

For each criterion: read the definition and scoring anchors, reason about how
the answer performs, then assign a score 0.0–1.0.

---

### `grounded`

Every non-trivial factual claim traces to a URL, page, or search result the
agent actually fetched. General knowledge or prior training does not count as
"retrieved" — claims must be grounded in content retrieved during this session.

If `web_fetch` returned errors for most or all sources, the agent should have
stated this explicitly. If it did, cap `grounded` at **0.5** (honest about
limits). If it synthesised silently despite failed fetches, score **0.0**.

- **1.0** — every non-trivial claim links to a retrieved source
- **0.5** — most claims are traceable; a few assertions lack retrieved backing, or the agent honestly disclosed fetch failures
- **0.0** — the majority of claims have no retrieved backing and no disclaimer

---

### `no_fabrication`

Judge whether anything in the answer reads as **invented** — a specific (name,
price, address, rating, date, statistic) stated with false confidence that the
answer's own reasoning doesn't support, or a quote/figure that looks made up.
Score this on the answer's own merits: internal plausibility and consistency.

**Do not try to verify which URLs were fetched** — whether each citation is
backed by retrieval is checked separately by deterministic code (it grades each
cited URL against what was actually fetched/searched), so don't second-guess a
URL's realness here. Focus only on invented-looking specifics.

- **1.0** — nothing reads as invented; specifics are presented with appropriate confidence
- **0.5** — minor secondary details look approximate or loosely stated
- **0.0** — a name, price, or statistic is clearly fabricated or wildly unsupported by the answer's own evidence

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

Score only **whether claims carry followable links at all** — a web-researcher
answer should attach inline URLs, not just name sources ("According to
TechCrunch…"), since a name can't be checked but a link can.

**Note:** the *quality* of those links — whether each cited URL was actually
fetched or searched this session — is graded separately by deterministic code
and overrides this criterion's score. So here, judge only the presence/absence
of links; don't reason about whether a given URL is real.

- **1.0** — every non-trivial claim has an inline URL
- **0.5** — URLs for most claims; a few unreferenced
- **0.0** — no URLs cited, only source names with no links

---

## Date-awareness

The agent has a `current_date` tool and is expected to call it before
researching time-sensitive topics. If the answer references events or data that
are clearly from the wrong year (e.g., searches framed around a year that has
already passed), treat this as a `grounded` failure — the retrieval was
mis-scoped. It is not a `no_fabrication` failure unless specific wrong details
were invented.

## Zero-retrieval handling

If the agent explicitly states it could not retrieve sources (all fetches
errored, bot-walls blocked every URL, no search results), score `grounded` and
`cites_sources` at **0.0** but grade `answers_question` and
`internally_consistent` on the honesty and completeness of the disclosure
itself, not on the absent content. If the agent synthesised silently, apply
hard caps as normal.

## Aggregation rule

1. Compute the arithmetic mean of the five criterion scores.
2. **Hard cap**: if `cites_sources` scores **0.0**, the overall score must not
   exceed **0.40**. (Note: `cites_sources` is set deterministically by code from
   how well each cited URL is backed by retrieval — so this cap fires only when
   the answer's citations genuinely aren't backed, not on a judge's guess.)
3. **Hard cap**: if `no_fabrication` scores **0.0** (a clearly invented specific),
   the overall score must not exceed **0.35** — an answer with fabricated
   specifics is actively harmful.
4. Report the most restrictive cap (if any) as `score`.

`feedback` must name the specific failing criterion and what concretely would
fix it so the next revision can act on it.
