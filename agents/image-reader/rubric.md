# Image reader rubric

The image reader transcribes or extracts text from attached images - handwriting, dense documents, small or degraded print. It has no retrieval tools; everything must come from the attached image. The independent judge evaluates each answer by scoring the criteria below - each on a **0–10 integer scale** - reasoning explicitly before assigning each score.

## Judge capabilities

**Images:** The judge receives the original image alongside the answer and can directly compare claims against the source. Evaluate all criteria by inspecting the image directly.

This agent is image-only. If an audio file appears, treat the answer as unanswerable and score `answers_question` at 0.

---

## How to score (G-Eval)

Work through the criteria **in order**. For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those steps - cite the specific passage or omission that drives your score.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's own scoring bands.

Score **substance, not style**: a long, fluent, confident-sounding answer is not automatically better than a short, plain one.

### The 0–10 scale

The same scale applies to every criterion. The per-criterion **scoring bands** tell you what "met", "partially met", and "failed" mean *for that criterion*; this scale tells you which number within those ranges to pick.

- **10** - flawless on this criterion; you can find nothing to fault.
- **9** - met; only a trivial, cosmetic nitpick.
- **8** - met; one minor gap that a careful reader would notice but that does not weaken the answer.
- **7** - met, but barely; a real (if small) shortcoming. *This is the lowest passing score - the gate's threshold sits here.*
- **6** - partially met; a noticeable weakness that should be fixed before this is acceptable.
- **5** - partially met; the weakness is squarely in the middle - as much wrong as right.
- **4** - mostly unmet; the criterion is more violated than satisfied.
- **3** - largely failed; a serious problem on this dimension.
- **2** - failed; the criterion is essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or the thing this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met more completely or the flaw is more trivial; the lower number when it only just clears the band. Do not default to 0, 5, or 10 - if the answer sits between two levels, choose the one whose description fits the dominant impression.

---

### `faithful`

Every statement in the answer traces to something visibly present in the image. Over-inference - drawing conclusions the image does not support - fails this criterion, even when no specific detail is fabricated.

**Evaluation steps.**
1. Inspect the image directly. Identify every claim in the answer (transcribed text, described objects, layout, relationships).
2. For each claim, verify it against what is actually visible. Flag any claim that goes beyond what the image shows or infers a relationship the image does not establish.
3. Check whether genuinely illegible text is flagged or given as a best-guess (acceptable) versus confidently asserted as exact (not acceptable).
4. Score the proportion of claims that are directly traceable to what you can see.

**Stale-knowledge caveat:** the agent may transcribe proper nouns, technical terms, or names that appear in the image but are unfamiliar to you. Do not flag a transcribed word as hallucinated merely because you do not recognise it - only flag it if the image clearly shows different text.

**Scoring bands.**
- **7–10** - essentially every claim is traceable to the image; uncertain or unclear items are flagged or qualified.
- **4–6** - most claims are grounded, but a few specifics or inferences are asserted without hedging where the image was ambiguous.
- **0–3** - the answer draws conclusions or fills in details the image could not support, or claims directly contradict what is visible.

---

### `no_hallucination`

The answer contains no detail invented from the model's training knowledge - no specific name, number, date, or quoted phrase stated confidently that is not present in the image. This criterion targets commission errors: things added that were not there, not omissions.

Do **not** score a transcribed proper noun as hallucinated merely because you do not recognise it. A name or term unfamiliar to you may be exactly what the image contains. A specific is suspect only when the answer is internally inconsistent, or when it asserts a detail with no hedge in a context where it could not have been read from the image.

**Evaluation steps.**
1. List every named entity, number, or quoted phrase in the answer.
2. Look for each in the image. Flag any that appear confident yet are absent from or contradict what you see.
3. Pay attention to specifics that could plausibly be generated from training knowledge rather than read from the image - a person's name inferred from appearance, a date not visible, a brand not shown.

**Scoring bands.**
- **7–10** - no specifics appear invented from training knowledge; where the model infers, it says so.
- **4–6** - minor secondary details look approximate or loosely inferred; no core name, date, or quoted phrase appears fabricated.
- **0–3** - a name, number, or quoted phrase is clearly not from the image and is stated as fact without qualification.

---

### `completeness`

The answer captures all significant text and content in the image - it does not silently omit sections, labels, or passages. This criterion is distinct from `faithful`: `faithful` asks whether stated claims are supported; `completeness` asks whether important content was missed.

**Evaluation steps.**
1. Scan the image for distinct text regions, labels, and content sections.
2. Check whether each region is represented in the answer. Note any silently dropped section.
3. For dense images, check that small print and secondary labels are included where the user asked for full extraction.
4. Do not penalise for omitting content the user did not ask for (e.g., if the user asked only to transcribe a heading, body text can be omitted).

**Scoring bands.**
- **7–10** - all significant content the user asked for is present; minor incidental text may be omitted without penalty.
- **4–6** - a section or label is missing that a thorough reader would include.
- **0–3** - large portions of asked-for content are silently missing.

---

### `answers_question`

The response addresses exactly what the user asked, in full - not a related-but-different question, and not a partial answer that drops part of the request.

**Evaluation steps.**
1. Decompose the user's question into its distinct asks (e.g. "transcribe AND summarise AND list action items").
2. Check that each ask is addressed in the answer.
3. Note any silent narrowing or dropped sub-request.

**Scoring bands.**
- **7–10** - addresses the request completely, including all sub-asks.
- **4–6** - addresses the main ask but leaves a minor sub-request unanswered.
- **0–3** - misses the core ask or redirects to something different.

---

### `clean_output`

The response is ONLY the answer - it begins directly and ends with it. No preamble ("I can see…", "Let me analyse…"), no process narration, no meta-commentary about image quality or the agent's capabilities. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion written out more than once on the way to a final version.

**Evaluation steps.**
1. Read the first sentence: does the answer begin directly, or with a preamble?
2. Scan the body and tail for leaked planning, self-talk, or meta-commentary.
3. Check whether any snippet, list, or conclusion appears more than once in different (superseded) forms - a sign of an abandoned draft left in place.
4. Score down for each intrusion.

**Scoring bands.**
- **7–10** - pure answer; no preamble, narration, or trailing reasoning; only the final version of any content appears.
- **4–6** - a stray opener or a single meta sentence, otherwise clean.
- **0–3** - noticeable preamble, leaked planning/reasoning, or a duplicated/superseded draft left in the output.

---

## No-text handling

If the image contains no legible text and the answer honestly states this (e.g. "No text is visible in this image"), do not penalise `completeness`. Score `completeness` on whether the disclosure is accurate and the description of what IS visible is reasonable. Score `answers_question` on whether the honest disclosure is an appropriate response to the request.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to 0.0–1.0 (divide by 10). The overall score is the **lowest** criterion - the binding constraint (weakest-link gating). There is **no averaging and no caps**: a single failing criterion sinks the answer on its own.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them so the next revision can act on it.
