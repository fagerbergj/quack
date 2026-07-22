# Media reader rubric

The media reader extracts, transcribes, or describes content from attached images and audio. It has no retrieval tools - everything must come from the attached media. The independent judge evaluates each answer by scoring the criteria below - each on a **0–10 integer scale** - reasoning explicitly before assigning each score.

## Judge capabilities

**Images:** The judge receives the original image alongside the answer and can directly compare claims against the source. Evaluate `faithful` and `no_hallucination` by inspecting the image directly.

**Audio:** The judge cannot hear audio. For audio-only inputs, evaluate `faithful` and `no_hallucination` by checking whether the answer hedges appropriately, flags uncertain passages, and avoids confident assertion of specifics (names, numbers, quoted phrases) that cannot be verified. Do **not** penalise an answer merely because you cannot personally confirm a transcribed detail - the absence of your perception is not evidence of fabrication.

When the input contains both an image and audio (e.g. a screenshot paired with a transcript), apply direct image verification to visual claims and hedging-based evaluation to audio-derived claims.

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

Every statement in the answer traces to something visibly or audibly present in the media. Over-inference - drawing conclusions the media does not support - fails this criterion, even when no specific detail is fabricated.

**Evaluation steps - image input (you can see the image).**
1. Inspect the image directly. Identify every claim in the answer (described objects, text, layout, relationships).
2. For each claim, verify it against what is actually visible. Flag any claim that goes beyond what the image shows or infers a relationship the image does not establish.
3. Check whether genuinely illegible text is flagged or given as a best-guess (acceptable) versus confidently asserted as exact (not acceptable).
4. Score the proportion of claims that are directly traceable to what you can see.

**Evaluation steps - audio input (you cannot hear the audio).**
1. Identify every claim in the answer (described content, quoted phrases, characterised events).
2. For each, ask: does the answer present this as extracted from the audio, or does it assert something the audio would have to support?
3. Check whether inaudible or unclear passages are flagged inline rather than silently presented as certain.
4. Score the proportion of claims that appear appropriately grounded or hedged rather than baldly asserted.

**Scoring bands.**
- **7–10** - essentially every claim is traceable to the media; uncertain or unclear items are flagged or qualified.
- **4–6** - most claims are grounded, but a few specifics or inferences are asserted without hedging where the media was likely ambiguous.
- **0–3** - the answer draws conclusions or fills in details the media could not support, or claims directly contradict what is visible.

---

### `no_hallucination`

The answer contains no detail invented from the model's training knowledge - no specific name, number, date, or quoted phrase stated confidently that is not present in the media. This criterion targets commission errors: things added that were not there, not omissions.

Do **not** score a proper noun as hallucinated merely because you do not recognise it. A name or term you are unfamiliar with may be exactly what the media contains. A specific is suspect only when the answer's own text is internally inconsistent, or when the model asserts it with no hedge in a context where it could not have read it from the media.

**Evaluation steps - image input (you can see the image).**
1. List every named entity, number, or quoted phrase in the answer.
2. Look for each in the image. Flag any that appear confident yet are absent from or contradict what you see.
3. Pay particular attention to specifics that could plausibly be generated from training knowledge rather than read from the image (a person's name inferred from appearance, a date not visible, a brand not shown).

**Evaluation steps - audio input (you cannot hear the audio).**
1. List every named entity, number, or quoted phrase in the answer.
2. For each, ask: is this the kind of specific that must come from the audio, or could the model have drawn it from training data?
3. Flag any specific stated as fact - without any hedge - that the model could not have known without hearing it.

**Scoring bands.**
- **7–10** - no specifics appear invented from training knowledge; where the model infers, it says so.
- **4–6** - minor secondary details look approximate or loosely inferred; no core name, date, or quoted phrase appears fabricated.
- **0–3** - a name, number, or quoted phrase is clearly not from the media and is stated as fact without qualification.

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

### `internally_consistent`

The answer does not contradict itself, and its conclusions follow from the content it presents. This criterion is about self-contradiction - not about faithfulness to the media (which `faithful` covers).

**Evaluation steps.**
1. Check for self-contradiction: a claim in one section undercut by a claim in another.
2. Check that structured sections (summary vs. detail, list items vs. conclusion) agree with each other.
3. Check that hedges are consistent - if the answer says "possibly X" in one place, it should not assert X as certain later.

**Scoring bands.**
- **7–10** - fully consistent throughout; no internal contradictions.
- **4–6** - minor tensions that do not undermine the core answer.
- **0–3** - clear contradictions, or conclusions the presented content does not support.

---

### `clean_output`

The response is ONLY the answer - it begins directly with the answer and ends with it. No preamble ("I can see…", "Let me analyse…"), no process narration, no meta-commentary about the media format or the agent's capabilities. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion written out more than once on the way to a final version.

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

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to 0.0–1.0 (divide by 10). The overall score is the **lowest** criterion - the binding constraint (weakest-link gating). There is **no averaging and no caps**: a single failing criterion sinks the answer on its own.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them so the next revision can act on it.
