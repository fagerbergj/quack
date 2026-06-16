---
name: write-judge-rubric
description: |
  Writes LLM-as-a-judge rubric files: the named criteria, evaluation steps, and scoring bands
  that an independent judge model uses to score agent output. Covers criterion design (name,
  description, caveats, evaluation steps, scoring bands), invariant sections (scale, aggregation,
  feedback rules), and when to write a per-agent override vs. rely on the default rubric.
  Use when creating or auditing any rubric.md file for an agent bundle or the global default.
license: MIT
---

# Write Judge Rubric

## Checklist (validate before shipping)

- [ ] Each criterion has a name (code identifier), description, evaluation steps, and scoring bands
- [ ] Evaluation steps are concrete, ordered, and actionable — not restatements of the description
- [ ] Scoring bands cover three ranges (high / partial / fail) with specific, observable conditions
- [ ] Caveats are included wherever the judge's own knowledge or capabilities could mislead it
- [ ] The 0–10 scale block is present and unchanged from the standard
- [ ] The aggregation block is present: weakest-link gating, no averaging, feedback names the lowest criterion
- [ ] Criteria are **independent** — no criterion's score should logically depend on another's
- [ ] Domain-specific handling sections are included where retrieval, media, or date-awareness apply
- [ ] The rubric can be read cold by a judge that has never seen the agent's session

---

## Phase 1 — Gather Inputs

Before writing:

- **What does this agent do?** (web research, media extraction, synthesis, code review, etc.)
- **What can go wrong that the judge must catch?** Fabrication, missing citations, leaked reasoning, off-topic answers, hallucinated specifics?
- **What tools does the agent have?** Shapes which criteria apply — a retrieval agent needs `grounded` and `cites_sources`; a media agent needs `faithful` and `no_hallucination`; a pure-reasoning agent needs neither.
- **What does the judge have access to?** Can it see attached images? Can it hear audio? Can it verify URLs? Sets the caveats each criterion needs.
- **Is this a per-agent override or the global default?** Per-agent rubrics replace the default entirely. Extend the default's shared criteria (clean_output, answers_question, internally_consistent) rather than rewriting them unless the domain genuinely requires different wording.

---

## Phase 2 — Criterion Design

Each criterion has five parts:

### 1. Name
A short code identifier in `snake_case`. The judge model calls this out by name in its feedback, and the gate matches it to the verdict structure. Keep it stable — renaming breaks existing verdict records.

### 2. Description
What the criterion is measuring, in a single tight paragraph. State what counts and what does not. Include the most important caveat inline if it is short (e.g., "Judge grounding by whether a claim carries an inline citation — NOT by whether you personally believe the cited fact"). For longer caveats, add a separate block after the description.

### 3. Caveats
Include whenever the judge's own knowledge, capabilities, or tendencies could mislead it:
- **Stale knowledge:** the agent retrieved live content the judge cannot see; recent or unfamiliar specifics are not evidence of fabrication.
- **No retrieval log:** the judge cannot see what URLs were fetched; score grounding by inline citations only.
- **No audio:** for audio inputs, the judge cannot hear; instruct it to evaluate hedging instead of content.
- **Deterministic override:** if a criterion's score will be overridden by deterministic code (e.g., citation backing), say so explicitly so the judge does not waste reasoning on it.

### 4. Evaluation Steps
Ordered, concrete sub-instructions the judge follows before assigning a score. This is the CoT layer — it forces deliberate reasoning rather than a snap judgment. Each step should be a specific action ("List the answer's non-trivial factual claims", "Check whether each claim carries an inline citation"). See `references/g-eval-method.md` for why evaluation steps matter and how to generate them.

For agents with multiple input modalities (image + audio), write separate step blocks per modality.

### 5. Scoring Bands
Three ranges covering the full 0–10 scale:
- **7–10** — criterion met (the threshold sits at 7)
- **4–6** — criterion partially met
- **0–3** — criterion failed

Each band must describe **observable, specific conditions** — not "good", "okay", "bad". The judge must be able to look at the answer and match it to a band without needing to interpret vague language.

---

## Phase 3 — Invariant Sections

These sections appear in every rubric, unchanged:

### The 0–10 scale block
Copy the standard scale from `assets/rubric-template.md`. The scoring bands in each criterion tell the judge what "met", "partially met", and "failed" mean for *that criterion*; the scale tells it which integer to pick within those ranges. Never alter the scale between rubrics — inconsistency breaks the gate's normalisation.

### Aggregation block
Always end the rubric with:
- Each criterion is independent, scored 0–10, normalised to 0–1 (÷ 10)
- Overall score = the **lowest** criterion (weakest-link gating)
- No averaging; no caps; one failing criterion sinks the answer
- `feedback` must name the lowest-scoring criterion/criteria and what concretely would fix them

---

## Phase 4 — Domain Handling Sections

Add these between the criteria and the aggregation block when relevant:

**Zero-retrieval handling** (retrieval agents): if the agent reports it could not retrieve sources, score `grounded` and `cites_sources` at 0 but do not penalise `answers_question` or `internally_consistent` — grade those on the honesty of the disclosure.

**Date-awareness** (retrieval agents with `current_date` tool): if the answer's searches are scoped to a wrong year, treat as a `grounded` failure (mis-scoped retrieval), not `no_fabrication`.

**Judge capabilities** (media agents): state explicitly what the judge can and cannot perceive — it can see attached images, it cannot hear audio. This determines which evaluation-step block applies per criterion.

---

## Resources

- Copy the blank structure from `assets/rubric-template.md` as a starting point.
- Read `references/g-eval-method.md` for G-Eval scoring mechanics: probability-weighted scoring, why CoT steps are critical, and how to generate evaluation steps automatically.

---

*For the evaluation strategy itself (how the gate invokes the judge, threshold config, revision loops), see `config/quack.yaml` and `docs/configuration.md`.*
