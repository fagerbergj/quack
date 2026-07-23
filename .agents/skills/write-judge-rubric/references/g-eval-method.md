# G-Eval - Scoring Method Reference

Load this file when writing evaluation steps for G-Eval rubrics, or when deciding how to structure scoring bands and scale.

---

## What G-Eval Is

G-Eval (Liu et al., EMNLP 2023) is a form-filling LLM-as-a-judge framework. The judge receives a criterion definition, auto-generated chain-of-thought evaluation steps, and the output to score, then fills in a structured form with an integer score. Scores are derived from the model's token log-probabilities (probability-weighted scoring) rather than the sampled integer - this prevents score clustering and captures finer quality distinctions, especially important for smaller judge models.

---

## Why Evaluation Steps Matter

Removing CoT evaluation steps drops Spearman correlation with human judgments from **0.514 → 0.500** on summarization. The effect is larger on smaller models: GPT-3.5 drops from **0.401 → 0.346** without CoT. Always include explicit, ordered evaluation steps before scoring.

Auto-generation: prompt the judge with the criterion definition and ask it to generate evaluation steps. Review, refine, then promote to fixed steps for reproducible runs - fixed steps are preferred over re-generating each time.

---

## The Form-Filling Prompt Pattern

```
You will be given one [output type] written for a [task type].
Your task is to rate it on one metric.

Evaluation Criteria:
{criterion_name} (1–10) - {criterion_description}

Evaluation Steps:
{numbered_steps}

[Input/context fields]

Evaluation Form (scores ONLY):
- {criterion_name}:
```

The judge outputs a score (or score + brief reason), not free-form text. The gate reads the log-probabilities of the candidate tokens (1–10) and computes a weighted average - not a hard discrete selection.

---

## Rubric Types

| Type | Structure | When to Use |
|---|---|---|
| **Analytic** | N independently scored dimensions | Default; isolates failure modes per criterion |
| **Holistic** | Single aggregate score | Simple pass/fail tasks where decomposition adds no signal |
| **Atomic** | Binary proposition checklist | When each criterion is a yes/no fact-check |

G-Eval defaults to analytic. The rubric files in this repo use analytic rubrics.

---

## Evaluative Anchor Types

When writing criterion descriptions, anchor them to one of:
- **Task-Grounded** - derived from the task's own instructions ("check all constraints from the prompt")
- **Behavior-Grounded** - based on observable output patterns ("no preamble or process narration")
- **Knowledge-Grounded** - anchored in domain standards or facts ("claims align with OWASP Top 10")

Most criteria in this repo are Task-Grounded or Behavior-Grounded. Knowledge-Grounded criteria require the judge to have reliable domain knowledge - use cautiously with local judge models.

---

## Scoring Bands vs. the 0–10 Scale

The 0–10 scale (10 = flawless, 7 = lowest passing, 0 = total failure) is fixed and shared across all criteria. The per-criterion **scoring bands** tell the judge what "met / partially met / failed" means for that specific criterion - they map observable conditions to the three ranges (7–10, 4–6, 0–3), not to individual integers. The judge then picks the integer within the range using the global scale.

---

## Threshold

The gate in this repo uses **threshold 0.6** (= score 6 on the 0–10 scale, normalised). The rubric files set **7 as the lowest passing score** in the scale description, which aligns to 0.7 after normalisation - slightly above the gate threshold, giving a small margin. Do not change the scale's "lowest passing score" annotation without also reviewing the gate threshold in `config/quack.yaml`.

---

## Sources

- [G-Eval: NLG Evaluation using GPT-4 with Better Human Alignment (Liu et al., EMNLP 2023)](https://arxiv.org/abs/2303.16634)
- [G-Eval Simply Explained - Confident AI](https://www.confident-ai.com/blog/g-eval-the-definitive-guide)
- [G-Eval Metric - Galileo AI](https://galileo.ai/blog/g-eval-metric)
- [From Holistic Evaluation to Structured Criteria: Rubrics Across the Evolving LLM Landscape (arXiv 2026)](https://arxiv.org/html/2606.08625v1)
