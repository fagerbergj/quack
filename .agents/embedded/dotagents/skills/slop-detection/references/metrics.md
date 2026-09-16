# The metrics, exactly

Two of the four numbers in `SKILL.md` come from the SlopCodeBench paper and are reproduced here with their original definitions and calibration data. Quote these figures rather than inventing thresholds.

## Verbosity

```
verbosity = |flagged lines ∪ clone lines| / LOC
```

Bounded in [0, 1]. Two halves, deduplicated against each other before counting:

- **Flagged lines** - lines matched by 137 hand-written ast-grep rules for code that could be said more briefly. A line hit by several rules counts once. The rule set is handcrafted taste, not a standard; `references/patterns.md` carries the pattern families rather than the rules.
- **Clone lines** - lines inside structurally duplicated runs, normalised by LOC. This is the half `SKILL.md` measures, because it needs no rule corpus.

| Codebase | Verbosity |
|---|---|
| 48 maintained Python repositories | 0.15 ± 0.06 |
| Code written by coding agents | 0.33 ± 0.10 |
| The article author's own vibe-coded projects | up to 0.4 |

Only one of the 48 repositories exceeds the agent mean. Over an agent trajectory, the duplication half grows about 66% (in 72.1% of runs) while the ast-grep half grows 15.6% - duplication is where the movement is.

## Erosion (concentration)

```
mass(f)  = CC(f) × √SLOC(f)
erosion  = Σ mass(f) for f where CC(f) > 10   /   Σ mass(f) over all f
```

`CC` is McCabe cyclomatic complexity, `SLOC` the function's source lines. The square root compresses size so that complexity, not length, dominates the weight. The cutoff of 10 follows Radon's published complexity bands.

| Codebase | Erosion |
|---|---|
| 48 maintained Python repositories | 0.31 ± 0.17 |
| scipy / scikit-learn (the human ceiling in that panel) | 0.457 / 0.411 |
| Code written by coding agents | 0.68 ± 0.20 |
| The article author's own vibe-coded projects | up to 0.75 |

Two raw counters move with it and are cheaper to compute: the number of functions over CC 10 (mean 4.1 → 37.0 across a trajectory) and the maximum CC in the codebase (27.1 → 68.2). In one recorded run a `main()` went from CC 29 and 84 lines to CC 285 and 1099 lines over eight rounds.

`SKILL.md` applies both formulas to the functions a diff touches, not to the whole repository. Repository-wide figures charge a change for debt it did not create; the panel numbers above are still the right scale to compare a diff's changed functions against, since the concentration ratio is unitless.

## What the source study established, and what it did not

- **Established:** agent-written code averages roughly twice the verbosity and erosion of maintained human code, and both climb monotonically as an agent extends its own earlier work, while human repositories plateau.
- **Established:** the degradation compounds across rounds rather than coming from one bad round. The mechanism the paper argues is that early architectural choices propagate through the workspace each round inherits; the benchmark also withholds the previous round's conversation, but no experiment separates the two.
- **Established, and the reason these are numbers rather than prompt text:** a quality-aware prompt lowered starting verbosity by about a third on both models tested, and lowered erosion on 20 of 20 problems for GPT 5.4 and 18 of 20 for GPT 5.3 Codex, while changing the *rate* of degradation not at all. It raised cost per round on GPT 5.4; Codex saw no cost increase. Instructions move the intercept; only measurement moves the slope.
- **Not established:** any link to correctness. Halving erosion and cutting verbosity by a third left every pass-rate measure statistically unchanged (paired Wilcoxon). Across nine threshold and size-term variants, erosion's correlation with the next round's pass rate stays near zero; its correlation with the next round's *cost* is positive. These measure what code costs to keep, not whether it works.
- **Warned against by the article rather than the paper:** asking a model to score code quality. It describes 1-10 rating as close to a random number generator and cites work showing pairwise A/B judging flips its preference when the two solutions are renamed. Compute the number; do not ask for an opinion of it.
- **Established, and awkward for anyone ranking these metrics:** in the threshold sweep, plain LOC is by far the strongest predictor of the next round's cost (0.534), ahead of max CC (0.323) and well ahead of erosion itself (0.127). The concentration measures say more about *where* the cost sits than about how much of it there is.
- **Named but unmeasured:** coupling between functions, code churn, cohesion. The article lists these as directions it wants to explore. Do not cite them as if they carried numbers.

## Sources

- Sebastian, *If coding is solved, what now?: Measuring the sloppiness of code*, Earendil, 10 Sep 2026 - <https://earendil.com/posts/measuring-code-sloppiness/>
- *SlopCodeBench* - <https://arxiv.org/html/2603.24755v1> (definitions in §2.3, the prompt study in §4.3 and Appendix B, the threshold sweep in Appendix G)
- Radon complexity bands, for the CC cutoff - <https://radon.readthedocs.io/en/latest/intro.html>
- ast-grep, the tool the flagged-lines half is built on - <https://ast-grep.github.io/>
