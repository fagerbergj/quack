# Lineup efficiency

Source: `research-r8-history-grading-v2.md` §1(c).

## Definition

Lineup efficiency = actual starter points ÷ points of the best possible
*legal* lineup from the same roster that week (final scores, slot-legal,
each player once) × 100. The season figure is the **total-points-weighted
ratio** — never a simple average of weekly percentages
([ffwrapped, undated, current 2026](https://blog.ffwrapped.com/posts/fantasy-football-lineup-efficiency)).

## No universal threshold

No published distribution of lineup efficiency exists for a 10-team PPR
redraft league — say so explicitly rather than importing one. The
closest published number, N(77.5%, 5%), is a *simulation parameter* for
generating fake seasons, not a measurement of real managers
([ffsimulator R package docs](https://ffsimulator.ffverse.com/articles/custom)).
A 12-team keeper/dynasty sample (85.9%, 86.8%, 83.7% across three
seasons) explicitly argues there is no universal good/bad threshold, and
that a 10-team league with only 5 bench slots "should trend higher"
while a deep roster can produce *lower* efficiency because there are more
reasonable ways to be wrong ([Advanced Sports Logic, Sep 12, 2026](https://advancedsportslogic.com/nfl/6416-set-better-fantasy-football-lineups)).
Compute the norm from this league's own five seasons; never import a
12-team number as the threshold.

## Points left on the bench

Only the best legal substitution counts: optimal minus actual. Total
bench production is not the missed-points figure.

## Red flag = persistence

A single low-efficiency week is not a finding. Over six seasons, each
team's lost-opportunity vs. league average regressed toward zero with
extreme deviations of only ~±4 points/week
([Tony Elhabr, undated](https://tonyelhabr.rbind.io/posts/fantasy-football-performance/)).
Decision-error metrics are only meaningful aggregated over weeks —
Correct Decision Rate (CDR) = correct decisions ÷ rostered weeks
([Fantasy Genius, Oct 27, 2023](https://fantasygenius.substack.com/p/decision-disasters-a-statistical)).

## Efficiency is separable from wins

The league winner in the canonical "actual vs. best possible" study had
*bad* lineup-picking and won anyway — high efficiency does not visibly
correlate with record in a single season
([Steven Morse, Sep 15, 2019](https://stmorse.github.io/journal/fantasy-bench.html)).
Never equate a rate with a record in either direction.

## Bad-week checklist before calling it process

Before judging a low-efficiency week as a mistake: was the starter
already ruled out (a fixable process error), was a bye/empty
slot/role-change overlooked (fixable), or was the bench boom unpredicted
(not a real mistake)? Only the first two are actionable process
mistakes.
