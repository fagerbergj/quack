# [Agent name] rubric

[One sentence: what the agent does, what tools it has, what the judge is evaluating.]

[If the judge's perception is limited - e.g. cannot hear audio, cannot see retrieval log - state it here.]

---

## How to score

Work through the criteria **in order**.
For each one:

1. Read its definition and **evaluation steps**.
2. Reason in one or two sentences about how the answer performs against those steps - cite the specific passage or omission that drives your score.
3. Assign an **integer from 0 to 10** using the scale below and the criterion's own scoring bands.

Score **substance, not style**: length, fluency, and confident phrasing earn no credit on their own.

### The 0–10 scale

- **10** - flawless on this criterion; nothing to fault.
- **9** - met; only a trivial, cosmetic nitpick.
- **8** - met; one minor gap that does not weaken the answer.
- **7** - met, but barely; a real (if small) shortcoming. *Lowest passing score - the gate's threshold sits here.*
- **6** - partially met; a noticeable weakness that should be fixed.
- **5** - partially met; as much wrong as right.
- **4** - mostly unmet; more violated than satisfied.
- **3** - largely failed; a serious problem on this dimension.
- **2** - failed; essentially not satisfied.
- **1** - failed badly; actively wrong on this dimension.
- **0** - total failure, or what this criterion asks for is entirely absent.

**Choosing within a band:** pick the higher number when the criterion is met more completely or the flaw is more trivial; the lower number when it only just clears the band.
Do not default to 0, 5, or 10.

---

### `criterion_name`

[Description: what this criterion is measuring.
State what counts and what does not.
Include the key caveat inline if short.]

[Caveat block if needed - e.g. stale knowledge, no retrieval log, no audio, deterministic override.]

**Evaluation steps.**
1. [Concrete action the judge takes before scoring]
2. [Next step]
3. [Final step - assign score]

**Scoring bands.**
- **7–10** - [specific observable condition for "met"]
- **4–6** - [specific observable condition for "partially met"]
- **0–3** - [specific observable condition for "failed"]

---

### `criterion_name_2`

[Repeat the block above for each criterion.]

---

## [Domain handling section - include if applicable]

**Zero-retrieval handling:** If the agent explicitly states it could not retrieve sources, score `grounded` and `cites_sources` at 0 but do not penalise `answers_question` or `internally_consistent` - grade those on the honesty of the disclosure.

**Date-awareness:** If the answer's searches are scoped to a wrong year, treat as a `grounded` failure, not `no_fabrication`.

---

## Aggregation

Each criterion is an **independent requirement**, scored 0–10 and normalised to 0.0–1.0 (divide by 10).
The overall score is the **lowest** criterion - the binding constraint (weakest-link gating).
There is **no averaging and no caps**: one failing criterion sinks the answer on its own rather than being averaged away by strong scores elsewhere.
The gate passes only when **every** criterion clears the threshold.

`feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them so the next revision can act on it.
