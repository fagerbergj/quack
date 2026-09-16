---
name: slop-detection
description: >
  Measures how sloppy a change is with four numbers taken from the diff itself -
  lines added, duplicated-line ratio, cyclomatic complexity of each changed
  function, and the share of complexity concentrated above CC 10 - and names the
  verbose patterns no number catches. Use before committing, when reviewing a
  diff for duplication or over-complexity, when a codebase grows faster than the
  capability it delivers, or when asked to condense, simplify, or clean up code.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0"
---

# Slop detection: measure the diff, then cut what the numbers point at

Correct code can still be sloppy - duplicated, padded with lines that carry nothing, or piling new branches into functions that were already the biggest ones. Tests never see it. It is paid for later, by whoever extends the code next, which is usually you next round.

So measure it instead of arguing about it. Four numbers, all computed from the diff you are about to commit or review.

## The four numbers

**1. Lines added.** `git diff --shortstat <base>...HEAD`. There is no threshold, and it is the crudest of the four - it is also the one that predicts best what the next round will cost, ahead of both complexity measures below. A change that added far more lines than the capability it delivers has an explanation, and you should be able to state it in one sentence.

Never optimise for it. Golfing lines to make the number look good is its own kind of slop, and the measure stops meaning anything the moment it becomes a target. That tension is the reason it is reported rather than aimed at.

**2. Duplicated-line ratio.** Count added lines that sit inside a run of six or more lines repeating elsewhere in the diff or in the file it lands in, ignoring whitespace and comments; divide by lines added.

This is the half of verbosity that grows fastest when an agent extends its own earlier code, and the only one with an objective answer: a run either repeats or it does not. A repeated block is a function you did not extract. Exclude fixtures, table-driven test bodies, generated files and vendored paths before counting - repetition is their correct shape.

**3. Cyclomatic complexity of each changed function.** Decision points plus one: `if`, `else if`, `case`, `for`, `while`, `catch`, `&&`, `||`, `?:`. Compute it for every function the diff adds or modifies, not for the repository.

Over 10 is where a function is worth splitting. What matters more is the delta: a function you pushed from 12 to 18 is the finding, whatever its neighbours look like.

**4. Concentration.** Give each changed function `mass = CC x sqrt(SLOC)`, then take the share of total mass held by the functions over CC 10.

Maintained codebases sit near 0.3; code written by agents averages about 0.68. A high share when the change added no large function of its own means the new branches went into the function that was already carrying the most - the most common way a codebase erodes, one reasonable-looking patch at a time.

Read `references/metrics.md` before quoting any of these numbers, comparing against a codebase, or picking a threshold: it carries the exact definitions, the calibration figures they come from, and what the source study did and did not establish.

## What a bad number tells you to do

- **Duplication high** - extract the repeated run. If the copies differ in one value, that value is the parameter.
- **CC over 10 on a function you touched** - early return for the guard cases, extract the branch body, replace an if/else ladder with a table or a map.
- **Concentration high** - the new logic wants its own function. Adding to the biggest function is what the number is objecting to.
- **Lines added high, other three fine** - often correct. Say what the lines buy, and move on.

## Three ways these numbers mislead

- **A target destroys the measure.** Report them; never optimise for them.
- **They say nothing about correctness.** In the study behind them, cutting verbosity by a third and halving concentration left every pass-rate measure statistically unchanged. A clean number is not evidence the code works, and a bad number alone does not block a correct fix.
- **The rest of slop has no number.** Trivial wrappers, defensive `try`/`catch` around code that cannot fail, a cast to silence the type checker, a variable used once immediately after it is declared, comments narrating the next line. Read `references/patterns.md` when you are naming what to cut, or judging whether a pattern is sloppy or correct here - it lists each pattern with its replacement and the context where it is the right call.

## When NOT to use

- Generated code, vendored dependencies, lockfiles, migrations, fixtures. Excluded from every count, never findings.
- A diff whose branchiness is inherent - a parser, a state machine, a protocol decoder, an exhaustive `switch` over an enum. High CC there is the problem's shape, not the author's.
- As a blocking objection on someone else's change. These are `suggestion:`-grade findings with a number attached, not defects. A change that improves the codebase still merits approval with them outstanding.

## Validation loop

Compute the four numbers on your own diff, cut the worst one, recompute. Stop when a number stops moving or when cutting further would stop the change being the smallest one that works - whichever comes first. Quote the numbers you acted on, not every number you took.

## Resources

| File | Read it when |
|---|---|
| `references/metrics.md` | Quoting a number, comparing to a codebase, or choosing a threshold. |
| `references/patterns.md` | Naming what to cut, or deciding whether a verbose pattern is justified here. |
