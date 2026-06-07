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

Specific names (businesses, operators, venues, people), addresses, phone
numbers, prices, opening hours, speeds, distances, ratings, and URLs must
all appear verbatim in retrieved content — not inferred, interpolated, or
invented. Web retrieval makes fabrication especially easy to detect and
especially harmful because users act on the information.

**Any quantitative claim — a percentage, count, price, speed, rating, or time
value — must appear verbatim in fetched content.** A plausible-sounding number
with no retrieved source is fabrication, not approximation.

Business names, operator details, tour prices, and specific service
descriptions are **high-risk fabrication vectors**: treat any unverified
specific as a near-fail on this criterion.

When a **Source verification** section is provided before this question:
- A URL marked **NOT fetched** was cited but never successfully retrieved.
  Treat any specific detail attributed to that source as unverified, and
  score accordingly (near-0 if the unverified source carries load-bearing claims).
- For **fetched** URLs, cross-check the content sample against the cited
  claim. If the sample contradicts or does not support the claim, that claim
  is fabricated even though the URL itself is real.

- **1.0** — every specific and every quantitative claim is verified in fetched content
- **0.5** — minor secondary details may be approximate; no operator names, prices, addresses, or specific numbers are fabricated
- **0.0** — business names, prices, statistics, or any specific number appears without a fetched source, or a NOT-fetched URL carries load-bearing claims

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
names alone ("According to TechCrunch…") do not count — a name cannot be
checked, only a link can. For a web researcher, URLs are a baseline
requirement, not a bonus.

When a **Source verification** section is provided: a URL marked NOT fetched
counts as a missing citation for scoring purposes — citing a URL you did not
read is not attribution, it is misdirection.

- **1.0** — URLs provided for all non-trivial claims, and all cited URLs were fetched
- **0.5** — URLs for most claims; a few unreferenced or not fetched
- **0.0** — no URLs cited, only source names with no links, or every cited URL is marked NOT fetched

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
   exceed **0.40**, regardless of the mean.
3. **Hard cap**: if `no_fabrication` scores **0.0**, the overall score must not
   exceed **0.35**. An answer with fabricated business names or invented URLs
   is actively harmful — it fails harder than one that merely omits citations.
4. Report the most restrictive cap (if any) as `score`.

`feedback` must name the specific failing criterion and what concretely would
fix it so the next revision can act on it.
