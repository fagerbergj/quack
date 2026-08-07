# Synthesizer rubric

The synthesizer receives completed research findings and combines them into a single answer. It has no retrieval tools - all information must come from the research provided. This rubric evaluates whether the synthesizer faithfully combined what it was given, without inventing content or leaking process narration.

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those steps - cite the specific passage or omission that drives your score.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's own scoring bands.

Score **substance, not style**: length, fluency, and confident phrasing earn no credit on their own.

### The 0–10 scale

- **10** - flawless on this criterion; nothing to fault.
- **9** - met; only a trivial, cosmetic nitpick.
- **8** - met; one minor gap that does not weaken the answer.
- **7** - met, but barely; a real (if small) shortcoming. *Lowest passing score.*
- **6** - partially met; a noticeable weakness that should be fixed.
- **5** - partially met; as much wrong as right.
- **4** - mostly unmet; more violated than satisfied.
- **3** - largely failed; a serious problem on this dimension.
- **2** - failed; essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or what this criterion asks for is entirely absent.

---

### `no_fabrication`

The synthesizer must not introduce information not present in the research provided to it. Every specific (name, number, claim, URL) must trace back to the research it received - not to training-data knowledge.

**Caveat - judge's knowledge is stale.** The synthesizer received live research you cannot see. Do **NOT** flag a claim as fabricated merely because it is unfamiliar or postdates your training. A specific is "invented" only when the synthesizer's own output is internally inconsistent or makes a precise claim that its own stated evidence does not support.

**Evaluation steps.**
1. Identify the named specifics and factual claims in the synthesized answer.
2. For each, check whether the answer's own text - not your memory - supports the claim.
3. Flag claims stated with confidence that the answer's own evidence does not justify.

**Scoring bands.**
- **7–10** - all claims trace to the research presented; nothing reads as added from outside.
- **4–6** - minor secondary details look loosely stated; no core fact appears invented.
- **0–3** - a name, statistic, or claim is clearly unsupported by the answer's own evidence.

---

### `complete_synthesis`

The answer addresses every distinct part of the original question, drawing on all relevant research provided. No research output is silently ignored.

**Evaluation steps.**
1. Identify the original question's distinct parts or sub-questions.
2. Check whether each part is addressed in the synthesized answer.
3. Note any research angle that was visibly omitted or undercovered.

**Scoring bands.**
- **7–10** - every part of the question is addressed; all major research threads are incorporated.
- **4–6** - main ask addressed; one or two sub-questions left thin or missing.
- **0–3** - core parts of the question are missed, or major research outputs ignored.

---

### `cites_sources`

Every significant claim carries an inline citation to the source that backs it. The synthesizer doesn't create sources - it preserves the citations the research it received already carried, in whichever form that research used. Two forms are followable and count equally:

- a web URL, as an inline Markdown link (`[source text](https://exact-url)`);
- a repo-relative code citation, either `<repo>@path/to/file.go` (optionally `:start-end`) - the form code-explorer nodes hand it - or `[label](path/to/file.go#L42)`.

Which form applies depends on what the upstream research was: code exploration cites code, web research cites URLs, and a synthesis of both carries both. Do not expect a URL from research that never touched the web, and do not privilege one form over the other - check whether each claim has an inline citation in *either* form, not which regime the run belongs to.

Source names alone ("the code shows…", "According to Wikipedia…") do not count - a name cannot be followed, only a link or a path can. Whether a cited link/path actually resolves (a URL was fetched, or a code path was read) is checked separately by deterministic code and can override this score; here, judge only whether claims carry an inline citation in one of the two forms.

**Evaluation steps.**
1. Count the answer's non-trivial factual claims.
2. For each, check whether it carries an inline citation - a followable URL or a repo-relative code reference - next to the claim, not deferred to a Sources block.
3. A Sources/References block does not, by itself, back a claim it isn't attached to - a placeholder or unlinked entry there does not rescue an uncited claim.
4. Score the proportion of claims that carry an inline citation in either form.

**Scoring bands.**
- **7–10** - every non-trivial claim carries an inline citation, in whichever form (URL or code) its research used.
- **4–6** - most claims cited; a few are unreferenced.
- **0–3** - no inline citations in either form, or only source names / a Sources block with nothing inline to back the claims.

---

### `clean_output`

The response is ONLY the synthesized answer - it begins directly with the answer content (title or first sentence) and ends with the answer or its Sources block. No preamble, no process narration, no meta-commentary about skills or formatting rules. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion - a snippet, a list, a decision - written out more than once on the way to a final version.

**Evaluation steps.**
1. Read the first sentence: direct answer content, or a preamble ("Let me compile…", "Based on the research…")?
2. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
3. Check whether any snippet, list, or conclusion appears more than once in different (superseded) forms - a sign of an abandoned draft left in place.
4. Score down for each intrusion.

**Scoring bands.**
- **7–10** - pure answer; no preamble or trailing narration; only the final version of any content appears.
- **4–6** - a single stray opener or one meta sentence, otherwise clean.
- **0–3** - noticeable preamble, leaked planning in the output, or a duplicated/superseded draft left in place.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to 0.0–1.0 (divide by 10). The overall score is the **lowest** criterion (weakest-link gating). There is **no averaging and no caps**: one failing criterion sinks the answer on its own.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them.
