---
name: write-judge-rubric
description: |
  Writes LLM-as-a-judge rubric files: the named criteria, evaluation steps, and scoring bands
  that an independent judge model uses to score agent output. Covers criterion design (name,
  description, caveats, evaluation steps, scoring bands), invariant sections (scale, aggregation,
  feedback rules), and when to write a per-agent override vs. rely on the default rubric.
  Use when creating or auditing any rubric.yaml file for an agent bundle, or the global default
  at config/rubric.md (still markdown - not bundle-scoped, no per-agent conversion planned).
license: MIT
---

# Write Judge Rubric

Bundle rubrics are `agents/<name>/rubric.yaml`. The global default at `config/rubric.md`
is a separate, still-markdown file loaded by a different code path (`internal/config`'s
`RubricPath`) - do not convert it or describe it as YAML.

## Checklist (validate before shipping)

- [ ] Top-level `scale: {min, max, pass}` is present and unchanged from the standard (0/10/7)
- [ ] Each criterion under `criteria.<name>` has `definition`, `steps`, and `bands`
- [ ] `steps` are concrete, ordered, and actionable - not restatements of `definition`
- [ ] `bands` cover three ranges (high / partial / fail), each `{min, max, meaning}`, with specific, observable conditions in `meaning`
- [ ] `guidance` is included wherever the judge's own knowledge or capabilities could mislead it (stale knowledge, no retrieval log, no audio, deterministic override)
- [ ] `anchors` (e.g. `[quote, omission]`) are set so feedback points at specific evidence
- [ ] Criteria that are graded/overridden by deterministic code set `deterministic: true` and a `fix` string
- [ ] Criteria are **independent** - no criterion's score should logically depend on another's
- [ ] Domain-specific handling (zero-retrieval, date-awareness, judge capabilities) goes in the top-level `notes` block
- [ ] The rubric can be read cold by a judge that has never seen the agent's session

---

## Phase 1 - Gather Inputs

Before writing:

- **What does this agent do?** (web research, media extraction, synthesis, code review, etc.)
- **What can go wrong that the judge must catch?** Fabrication, missing citations, leaked reasoning, off-topic answers, hallucinated specifics?
- **What tools does the agent have?** Shapes which criteria apply - a retrieval agent needs `grounded` and `cites_sources`; a media agent needs `faithful` and `no_hallucination`; a pure-reasoning agent needs neither.
- **What does the judge have access to?** Can it see attached images? Can it hear audio? Can it verify URLs? Sets the caveats each criterion needs.
- **Is this a per-agent override or the global default?** Per-agent rubrics replace the default entirely. Extend the default's shared criteria (clean_output, answers_question, internally_consistent) rather than rewriting them unless the domain genuinely requires different wording.

---

## Phase 2 - Criterion Design

Each criterion is a key under top-level `criteria:`, in `snake_case` (e.g. `criteria.grounded`).
That name is what the judge calls out in feedback and the gate matches to the verdict structure -
keep it stable, renaming breaks existing verdict records. Each criterion has these fields (see a
converted example at `agents/web-researcher/rubric.yaml`):

### `definition` (required)
What the criterion is measuring, in a single tight paragraph (YAML block scalar, `>`). State what
counts and what does not. Include the most important caveat inline if it is short.

### `guidance` (optional)
Longer caveats live here, separate from `definition`. Include whenever the judge's own knowledge,
capabilities, or tendencies could mislead it:
- **Stale knowledge:** the agent retrieved live content the judge cannot see; recent or unfamiliar specifics are not evidence of fabrication.
- **No retrieval log:** the judge cannot see what URLs were fetched; score grounding by inline citations only.
- **No audio:** for audio inputs, the judge cannot hear; instruct it to evaluate hedging instead of content.
- **Deterministic override:** if a criterion's score will be overridden by deterministic code (e.g., citation backing), say so explicitly so the judge does not waste reasoning on it.

### `steps` (required)
A YAML list of ordered, concrete sub-instructions the judge follows before assigning a score. This
is the CoT layer - it forces deliberate reasoning rather than a snap judgment. Each item should be a
specific action ("List the answer's non-trivial factual claims", "Check whether each claim carries
an inline citation"). See `references/g-eval-method.md` for why evaluation steps matter and how to
generate them. For agents with multiple input modalities (image + audio), list separate steps per
modality.

### `bands` (required)
A list of `{min, max, meaning}` objects covering the full 0–10 scale, three ranges:
- **7–10** - criterion met (the threshold sits at 7)
- **4–6** - criterion partially met
- **0–3** - criterion failed

Each `meaning` must describe an **observable, specific condition** - not "good", "okay", "bad". The
judge must be able to look at the answer and match it to a band without needing to interpret vague
language.

### `anchors` (optional)
A list naming what kind of evidence feedback should point at, e.g. `[quote, omission]`.

### `deterministic` / `fix` (optional)
Set `deterministic: true` when a criterion's score gets overridden by deterministic code (e.g.
citation-backing checks) - the judge's own score is advisory. `fix:` is a short block scalar
describing what concretely resolves a failure (e.g. "Fetch each source, or remove the citation").

### Per-criterion `scale` (optional)
A criterion can carry its own `scale: {min, max, pass}` overriding the top-level one, when its
natural unit isn't 0–10 (e.g. `cites_sources` in `agents/web-researcher/rubric.yaml` scores a 0–1
proportion).

---

## Phase 3 - Invariant Sections

These appear in every rubric, unchanged:

### Top-level `scale` block
```yaml
scale:
  min: 0
  max: 10
  pass: 7
```
The bands in each criterion tell the judge what "met", "partially met", and "failed" mean for *that
criterion*; the scale tells it which integer to pick within those ranges and where the pass line
sits. Never alter this between rubrics - inconsistency breaks the gate's normalisation.

### Aggregation (implicit, not a YAML block)
The gate itself enforces this - it is not written into the file, but every rubric relies on it:
- Each criterion is independent, scored on its own scale, normalised to 0–1
- Overall score = the **lowest** criterion (weakest-link gating)
- No averaging; no caps; one failing criterion sinks the answer
- Judge feedback must name the lowest-scoring criterion/criteria and what concretely would fix them

---

## Phase 4 - Domain Handling: the top-level `notes` block

Domain-specific handling goes in an optional top-level `notes:` block scalar (see
`agents/web-researcher/rubric.yaml` for a real example), not a separate criterion:

**Zero-retrieval handling** (retrieval agents): if the agent reports it could not retrieve sources, score `grounded` and `cites_sources` at 0 but do not penalise `answers_question` or `internally_consistent` - grade those on the honesty of the disclosure.

**Date-awareness** (retrieval agents with `current_date` tool): if the answer's searches are scoped to a wrong year, treat as a `grounded` failure (mis-scoped retrieval), not `no_fabrication`.

**Judge capabilities** (media agents): state explicitly what the judge can and cannot perceive - it can see attached images, it cannot hear audio. This determines which `steps` apply per criterion.

---

## Resources

- Copy the blank structure from `assets/rubric-template.yaml` as a starting point.
- Read `references/g-eval-method.md` for G-Eval scoring mechanics: probability-weighted scoring, why CoT steps are critical, and how to generate evaluation steps automatically.

---

*For the evaluation strategy itself (how the gate invokes the judge, threshold config, revision loops), see `config/quack.yaml` and `docs/configuration/trust-gate.md`.*
