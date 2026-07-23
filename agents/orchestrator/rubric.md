# Orchestrator rubric

The orchestrator handles simple conversational queries directly and routes other work to the right specialist agents. This rubric applies to the orchestrator's **own text output** - direct answers to conversational queries. Routing decisions that reach the user only through a specialist's output are evaluated by that specialist's rubric.

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

### `answers_question`

The response addresses exactly what the user asked, in full. For routing decisions, the delegated response addresses the question; for direct answers, the orchestrator's own output addresses it.

**Evaluation steps.**
1. Identify what the user asked.
2. Check whether the response (direct or delegated) covers every part of the request.
3. Note any silent narrowing or redirection to a different question.

**Scoring bands.**
- **7–10** - addresses the request completely.
- **4–6** - covers the main ask; minor gaps.
- **0–3** - misses the core ask or redirects to a different question.

---

### `no_fabrication`

When the orchestrator answers directly (conversational queries), it does not invent facts, URLs, or specific details it cannot know from context.

**Evaluation steps.**
1. Identify claims the orchestrator made in its own direct response (not via a specialist).
2. Check whether each claim is answerable from context or is a plausible fabrication.
3. Weight specific claims (names, versions, URLs) most heavily.

**Scoring bands.**
- **7–10** - all direct claims are answerable from context; nothing invented.
- **4–6** - minor unverifiable details, but no load-bearing fabrication.
- **0–3** - a specific URL, claim, or fact is clearly invented or unverifiable.

---

### `clean_output`

The response begins directly with the answer - no preamble, no meta-commentary, no process narration. The orchestrator never exposes its routing logic to the user. This includes mid-body deliberation: visible self-correction ("Actually, let me reconsider…"), an abandoned or superseded draft left in place, or the same conclusion - a snippet, a list, a decision - written out more than once on the way to a final version.

**Evaluation steps.**
1. Read the first sentence: direct content, or a preamble / "let me route this to…"?
2. Scan for leaked routing logic, self-talk, or meta-commentary about skills or delegation.
3. Check whether any snippet, list, or conclusion appears more than once in different (superseded) forms - a sign of an abandoned draft left in place.
4. Score down for each intrusion.

**Scoring bands.**
- **7–10** - pure answer; no preamble or routing narration; only the final version of any content appears.
- **4–6** - a single stray opener or one routing comment, otherwise clean.
- **0–3** - noticeable preamble, routing narration, leaked process, or a duplicated/superseded draft left in the output.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to 0.0–1.0 (divide by 10). The overall score is the **lowest** criterion (weakest-link gating). There is **no averaging and no caps**: one failing criterion sinks the answer on its own.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them.
