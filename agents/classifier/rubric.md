# Classifier rubric

The classifier reads a cleaned document and emits a single strict JSON object —
`{tags, summary, confidence}` — for indexing and retrieval. The independent judge
scores the criteria below, each on a **0–10 integer scale**, reasoning explicitly
before each score. There are no sources to cite and no question to answer here:
judge the classification itself.

## How to score (G-Eval)

For each criterion in order: read its definition and evaluation steps, reason in
a sentence or two about how the output performs, then assign an integer 0–10
using the scale below and the criterion's bands. Score **substance, not style**.

### The 0–10 scale

- **10** — flawless on this criterion; nothing to fault.
- **9** — met; only a trivial nitpick.
- **8** — met; one minor gap.
- **7** — met, but barely; a real if small shortcoming. *Lowest passing score — the gate's threshold sits here.*
- **6** — partially met; a noticeable weakness to fix.
- **5** — partially met; as much wrong as right.
- **4** — mostly unmet.
- **3** — largely failed.
- **2** — failed.
- **1** — failed badly.
- **0** — total failure, or the thing asked for is entirely absent.

Pick the higher number when the criterion is met more completely, the lower when
it only just clears the band. Don't default to 0, 5, or 10.

---

### `valid_output`

The response is exactly one JSON object of the required shape and nothing else.

**Evaluation steps.**
1. Confirm the whole response is a single JSON object — no prose before/after, no markdown code fence.
2. Confirm it has `tags` (array of strings), `summary` (string), and `confidence` (`high`|`medium`|`low`), and no junk extra fields.
3. Confirm it parses as JSON.

**Scoring bands.**
- **7–10** — clean, parseable JSON of the exact shape.
- **4–6** — parseable but with stray prose/fence, a missing optional nicety, or a loose `confidence` value.
- **0–3** — not valid JSON, wrong shape, or wrapped in commentary.

---

### `tags_relevant`

The tags are what a future reader filters on, so judge them the way you'd judge a
classifier: **precision** (every tag is warranted by the document) and **recall**
(the set covers the document's real, filterable topics) — plus **consistency**, so
the same concept lands on the same tag and retrieval isn't fragmented.

**Evaluation steps.**
1. Check there are 2–6 tags, lowercase and hyphenated where multi-word.
2. **Precision**: every tag is specific and grounded in the document — no generic filler (`notes`, `document`), no topic the text doesn't support.
3. **Recall**: the set captures the document's primary subject, its form, and the main themes a reader would search by — nothing important left untagged.
4. **Consistency**: tags use conventional, normalized forms (a reader's natural query term), not idiosyncratic synonyms for a common concept.

**Scoring bands.**
- **7–10** — 2–6 specific, well-formed tags with good precision and recall over the document's topics.
- **4–6** — mostly fine but one or two are generic/unsupported (precision) or a key topic is untagged (recall), or the count/forms stray.
- **0–3** — generic, malformed, invented, or missing the document's actual subject.

---

### `summary_faithful`

The summary is a 1–3 sentence abstract that accurately says what the document is
and what it contains.

**Evaluation steps.**
1. Check length (1–3 sentences) and that it describes the document, not a tangent.
2. Judge faithfulness: every claim in the summary is supported by the document; nothing is invented.
3. Check it names the document's core subject rather than a peripheral detail.

**Scoring bands.**
- **7–10** — concise, accurate, captures the core subject; nothing invented.
- **4–6** — broadly accurate but vague, overlong, or misses the main point.
- **0–3** — inaccurate, invents content, or fails to describe the document.

---

### `confidence_honest`

The `confidence` value reflects the document's actual clarity.

**Evaluation steps.**
1. Judge how clear and unambiguous the document's topic and content are.
2. Check whether the stated `confidence` matches: `high` for clear/unambiguous, `medium` for a reasonable interpretation of partly-unclear content, `low` for significant ambiguity or missing context.

**Scoring bands.**
- **7–10** — confidence matches the document's actual clarity.
- **4–6** — somewhat miscalibrated (e.g. `high` on a genuinely ambiguous doc).
- **0–3** — badly miscalibrated.
