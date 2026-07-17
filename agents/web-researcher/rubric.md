# Web researcher rubric

The web researcher retrieves and synthesises live web content. It has access to
`current_date`, `web_search`, `web_fetch`, and `summarize` tools. The
independent judge evaluates each answer by scoring the criteria below — each on
a **0–10 integer scale** — reasoning explicitly before assigning each score.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those
   steps — cite the specific passage or omission that drives your score.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's
   own scoring bands.

Score **substance, not style**: a long, fluent, confident-sounding answer is not
automatically better than a short, plain one. Length and polish earn no credit.

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

---

### `grounded`

Every non-trivial factual claim traces to a URL, page, or search result the
agent actually fetched this session. General knowledge or prior training does
**not** count as retrieved.

You cannot see the agent's retrieval log, and your own knowledge may be stale or
incomplete. Judge grounding by whether each non-trivial claim **carries an inline
citation** to a source — treat a cited claim as grounded. Do **NOT** lower this
score because a cited fact is unfamiliar, recent, or absent from your own
knowledge; that is not evidence it is ungrounded.

**Evaluation steps.**
1. List the answer's non-trivial factual claims.
2. For each, check whether it carries an inline citation (an attribution to a
   retrieved page or search result), not whether you personally believe it.
3. If the agent states `web_fetch` failed for most/all sources, check whether it
   said so plainly. An honest disclosure of failed retrieval caps this criterion
   at **5** (not lower); silent synthesis despite failed fetches scores **0–2**.
4. Score the proportion of claims that carry a citation.

**Scoring bands.**
- **7–10** — essentially every non-trivial claim traces to retrieved material.
- **4–6** — most claims are traceable, but several lack retrieved backing, or
  the agent honestly disclosed that retrieval largely failed.
- **0–3** — the majority of claims have no retrieved backing and no disclaimer.

---

### `no_fabrication`

Judge whether anything reads as **invented**: a specific (name, price, address,
rating, date, statistic, quote) stated with false confidence that the answer's
own evidence and reasoning do not support. Web answers make fabrication both
easy to commit and harmful, because users act on the specifics.

**Do not** try to verify which URLs were fetched — citation backing is graded
separately by deterministic code, which overrides `cites_sources`. Here, judge
only invented-looking specifics on the answer's internal merits.

**Recency caveat — critical.** The agent retrieved live web content that you do
not have, and your own knowledge is stale and incomplete. Do **NOT** flag a
claim as fabricated merely because you don't recognize it, it sounds new, or it
postdates your training — an unfamiliar movie, show, product, person, or event
is **not** evidence of fabrication. A specific is "invented" only when the
answer's own text is internally inconsistent or makes a precise claim it never
supports — never because it conflicts with your memory. When in doubt about an
unfamiliar-but-cited specific, do not dock this criterion.

**Evaluation steps.**
1. Identify every specific quantitative or named claim (prices, counts, ratings,
   distances, hours, operator/business names, addresses).
2. For each, judge whether the answer presents it with confidence its own
   evidence does not justify — does it read as retrieved, or as plausibly
   invented?
3. Weight load-bearing specifics (the ones a reader would act on) most heavily.

**Scoring bands.**
- **7–10** — nothing reads as invented; specifics are stated with appropriate
  confidence.
- **4–6** — minor secondary details look approximate or loosely stated; no core
  name, price, or statistic appears fabricated.
- **0–3** — a name, price, or statistic is clearly fabricated or wildly
  unsupported by the answer's own evidence.

---

### `answers_question`

The response addresses exactly what the user asked, in full — not a
related-but-different question, and not a partial answer that drops part of the
request.

**Evaluation steps.**
1. Decompose the user's question into its distinct asks (including constraints
   like location, timeframe, budget, audience).
2. Check that each ask is addressed in the answer.
3. Note any silent narrowing or topic drift.

**Scoring bands.**
- **7–10** — addresses the request completely, including its constraints.
- **4–6** — addresses the main ask but leaves minor gaps or drops a constraint.
- **0–3** — misses the core ask or redirects to a different question.

---

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the
evidence it presents.

**Evaluation steps.**
1. Check for self-contradiction across the answer (a claim undercut later).
2. Check that each conclusion follows from the evidence the answer itself cites.
3. Check that uncertainty is stated where the evidence is thin, not papered over.

**Scoring bands.**
- **7–10** — fully consistent throughout; conclusions follow from the evidence.
- **4–6** — minor tensions that do not undermine the core conclusion.
- **0–3** — clear contradictions, or conclusions the evidence does not support.

---

### `cites_sources`

Score only **whether claims carry followable links at all** — a web-researcher
answer should attach inline URLs, not just name sources ("According to
TechCrunch…"), since a name cannot be checked but a link can.

**Note:** the *quality* of those links — whether each cited URL was actually
fetched or searched this session — is graded separately by deterministic code,
which **overrides** this criterion's score. So here, judge only the
presence/absence of links; do not reason about whether a given URL is real.

**Evaluation steps.**
1. Count the answer's non-trivial claims.
2. Count how many carry an inline, followable URL (not just a source name).
3. Score the proportion that are linked.

**Scoring bands.**
- **7–10** — every non-trivial claim has an inline URL.
- **4–6** — URLs for most claims; a few are unreferenced.
- **0–3** — no URLs cited, only source names with no links.

---

### `clean_output`

The response is ONLY the answer — it begins directly with the answer (its title
or first sentence) and ends with the answer (or its `Sources` section). No
preamble, no process narration, no planning or self-talk, no meta-commentary
about formatting/skills/rules, and no leftover reasoning. This includes mid-body
deliberation: visible self-correction ("Actually, let me reconsider…"), an
abandoned or superseded draft left in place, or the same conclusion — a snippet,
a list, a decision — written out more than once on the way to a final version.
The reader sees the reply verbatim, so anything like "Let me…", "I see, I made a
typo…", "Actually, wait…", "the skill says…", or trailing drafting notes is a
defect — even when the buried content is excellent.

**Evaluation steps.**
1. Read the first sentence: does the answer begin directly, or with a preamble /
   process narration?
2. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
3. Check whether any snippet, list, or conclusion appears more than once in
   different (superseded) forms — a sign of an abandoned draft left in place.
4. Score down for each intrusion; the buried answer's quality does not excuse it.

**Scoring bands.**
- **7–10** — pure answer; no preamble, narration, or trailing reasoning; only
  the final version of any content appears.
- **4–6** — a stray opener or a single meta sentence, otherwise clean.
- **0–3** — noticeable preamble, leaked planning/reasoning, or a
  duplicated/superseded draft left in the output.

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
`cites_sources` at **0** but grade `answers_question` and
`internally_consistent` on the honesty and completeness of the disclosure
itself, not on the absent content. If the agent synthesised silently despite
failed retrieval, score `grounded` at **0** as a hard failure.

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to
0.0–1.0 (divide by 10). The overall score is the **lowest** criterion — the
binding constraint (weakest-link gating). There is **no averaging and no caps**:
a single failing criterion sinks the answer on its own rather than being
averaged away by strong scores elsewhere, and a strong dimension never excuses a
weak one. The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely
would fix them so the next revision can act on it.
