---
name: history-grading
description: >
  Grading a Sleeper fantasy league's completed seasons and drafts in
  hindsight: draft pick value vs. end-of-season finish, lineup
  efficiency, luck vs. process, and how to present a report card without
  scolding. Load whenever the job is a season review, a draft grade, or
  naming a manager's recurring mistakes.
---

# History grading

Source: `research-r8-history-grading-v2.md` (10-team full-PPR Sleeper
league, single FLEX, QB/RB/RB/WR/WR/TE/FLEX/K/DEF + 5 bench, 6-of-10
playoffs from week 15). Every number below is as published, with its
source; where none exists, say so instead of inventing one.

## Decision procedure (run in this order)

1. **Declare your standards first.** State, in the output: the reach/
   steal band in use (default PFF's 20+/100+ spot bands), the
   replacement-level convention (bench/12th-man, last-starter, or
   waiver-wire — pick one, print which), and the lineup-efficiency
   denominator (total-points-weighted, never a simple average of weekly
   percentages). Load `reach-steal-definitions.md` and
   `pick-value-vs-finish.md`.
2. **Grade the draft.** Price each pick at its ADP slot; compare to
   end-of-season positional finish by PPG. Apply the healthy-play
   filter before calling a bust a draft error. Grade opportunity cost
   against the manager's *next* pick, not one other player. Check the
   declared positional shape (RB-heavy vs. Zero RB is the manager's own
   choice — hold them to it, don't grade against the other camp's rule).
   Load `positional-allocation-norms.md`.
3. **Grade lineup execution.** Compute season efficiency as the
   points-weighted ratio, never the average of weekly percentages.
   Never treat a single low-efficiency week as a finding — aggregate
   over weeks; extreme single-season deviations (~±4 pts/week) regress.
   Load `lineup-efficiency.md`.
4. **Separate luck from process.** Split every season into lineup
   execution (controllable), player-outcome luck, schedule luck
   (uncontrollable — PA is the sum of *opponents'* scores), and a
   pre-kickoff test before calling a specific week a skill error. Report
   the cumulative luck gap across seasons — it does not zero out. Never
   read close losses as clutch/anti-clutch evidence. Load
   `luck-vs-skill.md`.
5. **Write the card, not a scold.** Lead with net position (bench
   points, luck gap, net points vs. league), not a grade letter. Every
   criticism names the player, the pick/week, the number, and the rule
   it violates. Grade decision quality at decision time, not by
   outcome — a win never launders a wrong call. Load
   `report-card-presentation.md`.

## What bad advice looks like

A judge can check any sentence of the output against these:

1. A 12-team number (ADP, replacement baseline, efficiency norm) is used
   as this 10-team league's threshold with no adaptation flag.
2. "Reach"/"steal" stated without a declared band or a comparison to
   realized positional finish.
3. A single week, bust, or bad trade scolded as evidence of a bad
   manager, with no aggregation across weeks.
4. A losing season attributed to one cause only (draft quality, or "just
   bad schedule") while ignoring the other channel.
5. Total bench production presented as "points left on the bench"
   instead of optimal minus actual.
6. Efficiency and win/loss record conflated in either direction.
7. Close-loss rate used as clutch/anti-clutch evidence.
8. Schedule luck assumed to "even out" over five seasons instead of
   reported cumulatively.
9. A pre-kickoff decision graded with information the manager didn't
   have (outcome bias), or a roster penalized for opponent scoring.
10. An unsourced "data" figure (e.g. an undated site's proprietary
    numbers) quoted without its weak-source flag, or season efficiency
    computed as a simple average of weekly percentages.

## Resources (load only when the case calls for it)

- `pick-value-vs-finish.md` — grading a single pick: freeze-the-price
  method, healthy-play filter, opportunity cost (DLD), replacement-level
  conventions, slot-quality check.
- `reach-steal-definitions.md` — the declared reach/steal bands and
  which sources they come from.
- `positional-allocation-norms.md` — 10-team PPR shape norms, RB-heavy
  vs. Zero RB, QB/TE timing, K/DEF.
- `lineup-efficiency.md` — the efficiency formula, why there's no
  universal threshold, the bad-week checklist.
- `luck-vs-skill.md` — the four-channel decomposition, PF/PA, expected
  wins, close losses, the five-season persistence test.
- `report-card-presentation.md` — headline numbers, tone rules, the
  per-season card structure, and the five-season rollup.
- `output-schema.json` — the extension UI's schema for the `history`
  artifact. Write the artifact to match it exactly (see the agent
  prompt's Output section).
- `report.md` — the full R8 research report (v2), every finding with
  its full context. Load for a hard case, or to revise this skill.
