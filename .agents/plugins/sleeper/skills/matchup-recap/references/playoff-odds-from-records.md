# Playoff odds from records: the no-simulation method

Source: `report.md` (R4), section 1.4, 1.5, and disagreements (a)-(d), (i), (j).

This agent has no simulator. It computes the playoff line from Sleeper's
own standings/matchup data using the closed-form method below, states the
method in one line whenever it publishes a number, and never treats the
result as a guaranteed outcome.

## Step 1 — Pythagorean expectation

`Win% = PF^x / (PF^x + PA^x)`, multiplied by games played for expected wins.
The NFL exponent is **2.37** (Daryl Morey's fit at STATS Inc., used by
Football Outsiders since 2003). No dated fantasy-specific calibration of x
exists beyond one point: [Daniel Ludwinski, "Fantasy Playoff Probability"
(Nov 16, 2017)](https://dludwinski.com/blog/2017/11/16/fantasy-playoff-probability/)
used **x = 6**, "similar to what Yahoo uses in their projections," on a
comparable 10-team structure. **Fit x on this league's own completed season
once there's enough data; until then use x ≈ 4-6 and cite Ludwinski's 6 as
the only dated, named-expert fantasy anchor** — never state a different
fantasy exponent as settled.

## Step 2 — Regress toward .500

No published fantasy or NFL-midseason regression fraction exists — a
declared gap. The operating anchor, flagged as an assumption every time it's
used: NFL year-over-year regression credits a team with **~31% of its prior
wins above the mean** (`Wins = 5.51 + 0.31 × prior wins`, R² = 0.094 — Chase
Stuart, Football Perspective, Mar 25, 2015). Pythagorean wins regress less
than real wins (**0.3995** slope vs **0.3265**) because luck is already
partly removed.

## Step 3 — Project the final record with schedule folded in

`E(W) = wins so far + Σ p_i` over remaining games, where each `p_i` comes
from that specific opponent's projected points for/against — schedule
strength enters game-by-game rather than as a separate multiplier. In a
14-game fantasy season with fixed pairings, schedule matters more than in
the NFL's more balanced 17.

## Step 4 — The playoff line

Compute E(W) for all 10 teams; the **6th-largest projected win total is the
playoff line**. Always report the gap to the line ("0.4 wins clear"), never
a bare in/out flag.

## Step 5 — Probability, without a simulator

Treat remaining games as independent: `σ = sqrt(Σ p_i(1−p_i))`; for a 4-5
game finish, σ ≈ 1.1-1.4. `P(make playoffs) ≈ Φ((line_6 − E(W) + 0.5) / σ)`
(standard normal CDF, continuity correction) — this ignores schedule
correlation between teams, which is the reason real simulators exist.
**Sanity anchor:** Ludwinski's worked example on a comparable 10-team
structure produced 59.5% / 59.0% for two 5-5 teams fighting over two spots —
coin-flip odds at the cutoff.

## What real products do instead

Every published "playoff odds" tool simulates: ffwrapped runs 5,000
iterations, My Fantasy Analyzer 10,000, on the league's actual rosters and
schedule (both undated, retrieved 2026-09-18). This agent's closed-form math
approximates what those simulators draw from — say so rather than presenting
the closed-form number as equivalent to a simulation.

## Rules for publishing any percentage

**Rule M2** — a playoff-odds figure must be traceable to one of three
methods: Pythagorean projection, a base-rate table, or a named simulation,
never a vibes number. **Rule M3** — state the method in one line whenever a
percentage is published; "even a displayed 0% or 100% is a simulation
estimate" — never treat a high probability as a confirmed playoff berth
(ffwrapped, undated).

## No published base-rate table for this exact format

The closest published analog is 12-team/6-playoff, not 10-team/6-playoff —
a gap. If using it as a directional proxy, say so.
