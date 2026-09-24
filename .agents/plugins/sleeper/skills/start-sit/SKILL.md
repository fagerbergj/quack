---
name: start-sit
description: >
  Weekly start/sit decisions for a Sleeper fantasy roster: how to weigh
  projections, injury/practice status, matchup, game script, weather, and
  Vegas totals into a start or sit call per slot, including the single FLEX.
  Load whenever the job is "who should I start this week" or reviewing a
  lineup before kickoff.
---

# Start/sit decisions

Source: `research-r1-startsit.md` (10-team full-PPR Sleeper league, single
FLEX, QB/RB/RB/WR/WR/TE/FLEX/K/DEF + 5 bench, 6-of-10 playoffs from week 15).
Every number below is as published, with its source; where none exists, say
so instead of inventing one — see "No published number" below.

A lineup passed in as context is this chat's own earlier output, not a
constraint - every dedicated slot's incumbent is compared against the whole
bench at that position each run, not just a contingency named in a prior
round. `sleeper_matchup`'s `me` side carries a code-computed
`best_by_projection` baseline (the best legal lineup by projection, per
slot, Out/IR/Doubtful excluded); a starter that differs from it names the
baseline player and the evidence that beat it in the row's `why`.

## Decision procedure (run in this order per slot)

1. **Availability first.** Pull `injury_status` and `practice_description` via
   `sleeper_player`/`sleeper_roster`. A player carrying any designation is
   resolved before anything else — do not let a good matchup override an
   unresolved availability question. Load `practice-report-semantics.md` for
   the Wed/Thu/Fri interpretation and the injury-type play-rate table.
2. **Check outside news for a close call.** Sleeper's own designation and
   practice fields update on that reporting calendar, so they lag
   beat-writer and practice-report news between snapshots (see
   `sleeper:injury-and-news`). For a Questionable or limited-practice
   starter, or a bench player whose projection sits within the flex
   tie-break gap (`flex-decisions.md`, under 3 points) of the starter's,
   search the open web for what changed and cite it inline as a markdown
   link.
3. **Locks.** Sleeper locks each slot at that player's own kickoff
   (`sleeper_schedule` for kickoff times); a locked slot cannot change. A
   Thursday player has no Thu/Fri/Sat report — the decision is effectively
   final at the Wednesday 4pm ET report. State the lock time when a
   Questionable/Doubtful player's kickoff is close.
4. **Matchup, bounded.** Pull opponent defensive context. The real, quantified
   effect is small and position-dependent: −0.07 (QB), −0.13 (RB), −0.09 (WR)
   fantasy points per one-spot change in opponent defensive rank, TE
   statistically ~0 ([The Fantasy Footballers, Aug 3, 2021](https://www.thefantasyfootballers.com/articles/the-fantasy-football-mythbusters-making-the-most-of-matchups/)).
   Use it to break *close* calls only — never to bench a clearly-better
   player for a clearly-worse one on matchup alone ([The Athletic, Dec 24, 2024](https://www.nytimes.com/athletic/5744918/2024/12/24/fantasy-football-matchup-rankings-strength-of-schedule/)).
5. **Game script and weather, as a tiebreaker only.** Favorites win more
   (66% overall, 85% at 10+ points — [TFF, Jul 15, 2021](https://www.thefantasyfootballers.com/articles/the-fantasy-football-mythbusters-flip-the-game-script/)),
   but the same study's own verdict is that team-score projection dominates
   game-script narrative — pick the higher-scoring team's players rather than
   playing the script. Weather only matters at extremes (≥20-25mph sustained
   wind, heavy precipitation, <30°F) and trims efficiency, not volume — see
   `weather-and-vegas.md` for the full multiplier tables and the "no
   correlation" null result that also exists in the literature.
6. **FLEX last.** Fill the five dedicated slots first, then rank every
   remaining RB/WR/TE by the same criteria above. Load `flex-decisions.md`
   for the floor-vs-ceiling tie-break and the PPR-specific WR-lean data.
7. **Estimate floor and ceiling.** These are not calculated - weigh the
   matchup, the player's recent snap share and usage (`sleeper_player`'s `game_log`), and any
   team news into a low/high PPR estimate for the week, and link that
   evidence's source inline in the row's `why`. An inverted pair (floor
   above ceiling) is ignored by the renderer, so check the order before
   writing it.
8. **Write the verdict.** Every starter gets `start` or `sit` plus the
   `why`; the UI's `confidence` number is the chance the recommended player
   outscores the best alternative — leave it null when there is no real
   alternative (see agent prompt).

## Thresholds table (source-dated; do not restate without the URL)

| Signal | Number | Source |
| --- | --- | --- |
| Questionable → played (skill positions, 2017-2024) | 70.7% | [Footballguys, 09/13/2025](https://www.footballguys.com/article/2025-chance-to-play-questionable-vs-doubtful) |
| Doubtful → played (skill positions, same window) | 6.9% | same |
| Doubtful → played (all positions, 2017-2024) | 1.25% | [Banged Up Bills, 07/12/2025](https://bangedupbills.com/2025/07/12/how-do-the-buffalo-bills-compare-against-the-nfl-utilizing-the-questionable-designation/) |
| Final (Friday) practice FP → played | 86% | [Footballguys, 09/01/2024](https://www.footballguys.com/article/2024-injury-index-chance-to-play-practice-participation) |
| Final (Friday) practice LP → played | 71% | same |
| Final (Friday) practice DNP → played | 29% | same |
| Best-vs-worst run defense, RB PPG swing | ~4.2 pts across the backfield | [TFF, Aug 3, 2021](https://www.thefantasyfootballers.com/articles/the-fantasy-football-mythbusters-making-the-most-of-matchups/) |
| Team win rate as 10+ pt favorite | 85% | [TFF, Jul 15, 2021](https://www.thefantasyfootballers.com/articles/the-fantasy-football-mythbusters-flip-the-game-script/) |
| Role-collapse trigger (name stops mattering) | snap% <60% for 3+ games, or target share <12% over 4 games | [Fantasy Start/Sit, 2026](https://fantasystartsit.com/overvaluing-name-recognition/) |

Team-level variance is real and large (2024 Questionable play rates ranged
Tampa Bay 94.8% to Pittsburgh 22.2% — [Banged Up Bills](https://bangedupbills.com/2025/07/12/how-do-the-buffalo-bills-compare-against-the-nfl-utilizing-the-questionable-designation/));
treat the league-average numbers above as a prior, not a player-specific rate.

## When benching a star is justified

Justified: (a) genuinely unresolved availability with no contingency, (b) a
quantified role collapse (see trigger above), (c) return from a severe
injury with a limited-snaps track record. NOT justified on its own: a tough
matchup against an otherwise-healthy, clearly-better player — even
proponents of matchup adjustment concede this ("does not mean you bench a
stud with a tough APA for a flier with a good matchup" — The Athletic,
above). Heavy favorite → lean the higher floor; big underdog → lean the
higher ceiling ([Waiver Wizard, 2026](https://fantasywaiverwizard.com/learn/start-sit-decisions)).

## No published number

Several commonly-repeated thresholds have no primary source: a numeric
"sit if projection < X" rule, an exact wind/temperature sit line, a Vegas
total cutoff for switching RB/WR priority, and any W/T/F practice-sequence
play rate (e.g. "DNP-LP-LP"). When a case turns on one of these, say
explicitly that no published number exists and state the qualitative
reasoning instead — do not invent a figure to sound precise.

## Resources (load only when the case calls for it)

- `output-schema.json` — the extension UI's schema for the `lineup`
  artifact. Write the artifact to match it exactly (see the agent prompt's
  Output section for the required-field walkthrough).
- `practice-report-semantics.md` — DNP/LP/FP definitions, the injury-type
  play-rate breakdown (concussion/knee/hamstring), and why a practice report
  line is not itself an availability signal. Load whenever a starter carries
  any injury designation.
- `weather-and-vegas.md` — weather multiplier tables, the null-result study,
  and implied-team-total math. Load when the game is outdoors with a
  forecast worth checking, or when a Vegas total/spread is part of the case.
- `flex-decisions.md` — the FLEX-specific tie-break rules and PPR
  RB-vs-WR-value data. Load whenever the FLEX slot itself is the open
  question (not a fixed RB/WR/TE slot).
- `report.md` — the full R1 research report (54KB), every finding with its
  full context. Load for a hard case, or to revise this skill.
