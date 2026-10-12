---
name: draft-strategy
description: >
  Drafting well in a 10-team PPR Sleeper snake draft: tiers vs.
  rankings, ADP as a value signal, positional timing (RB/WR/QB/TE/K/
  DEF), positional runs, handcuffs and byes, pick-slot plans, and the
  60-90 second on-the-clock procedure. Load whenever the job is
  preparing a draft plan or advising a live pick.
---

# Draft strategy

Source: `research-r9-drafting.md` (10-team PPR redraft snake on Sleeper,
14 rounds, QB/RB/RB/WR/WR/TE/FLEX/K/DEF + 5 bench, rolling waivers, 100
FAAB, 6 playoff teams from week 15). Every number below is as published,
with its source; where none exists, say so instead of inventing one.

## Decision procedure (run in this order)

1. **Build the board on tiers, not a rank order.** A tier is a cluster
   of similar projected value bounded by real drop-offs; within a tier,
   preference is a coin flip, and tier width must be uneven. Load
   `tiers-vs-rankings.md`.
2. **Price against ADP, never draft from it.** ADP is the market's
   consensus price, a timing signal, not a projection. The reach test is
   VONA — if the target will survive to the next pick, reaching is not
   an edge. Never use a 12-team or other-platform ADP as this league's
   price. `sleeper_draft` already carries each pick's `adp_delta` and
   `adp_verdict` (`value`/`reach`/`fair`) — that verdict *is* the
   steal/reach call for a report card; never recompute it from `pick_no`
   and `adp`. Load `adp-as-value.md`.
3. **Time the position, not the round number.** RB/WR/QB/TE/K/DEF each
   have their own 10-team-specific timing evidence — load
   `roster-construction.md` for the position in question.
4. **Read the run, don't chase it.** Break at the tier cliff, regardless
   of run length; avoid being the end-of-run picker. Load
   `positional-runs.md`.
5. **Handcuffs and byes, checked not assumed.** A handcuff earns a pick
   only on the two-condition checklist; QB/TE byes must be unique. Load
   `handcuffs-and-byes.md`.
6. **Seat the plan to the slot.** Early/middle/late seats carry
   different wait times and plans — load `slot-plans.md`.
7. **On the clock: one answer, not a wall of rankings.** Run the
   60-90-second procedure — tier anchor, cliff check, VONA reach test,
   fallen-player check, run override, need-then-value, time-box. Load
   `on-the-clock-procedure.md` for the full sequence and what the output
   must and must never contain.

## What bad advice looks like

A judge can check any sentence of the output against these:

1. A player recommendation with no tier context.
2. Drafting to the list — the on-the-clock answer is "next on ADP/
   rankings" rather than a tier + need test.
3. Reaching across a tier cliff with no positional justification.
4. Joining a positional run while the position's tier still has depth
   (or refusing to break once it's exhausted).
5. Using 12-team, non-PPR, or other-platform ADP as this league's price.
6. Presenting stale ADP, injury status, or role claims with no snapshot
   timestamp.
7. Ignoring roster slots and byes — a QB2/TE2 sharing a bye with the
   starter, or a second early QB "for balance."
8. Spending a mid-late pick on a generic (healthy-starter) handcuff.
9. On-the-clock output that can't be executed in 60-90 seconds: a wall
   of rankings, no single recommended pick, no fallback.
10. A rigid pick-by-pick plan, forced positional balance, or advice that
    doesn't confirm the league's actual scoring/format/clock settings.

## Resources (load only when the case calls for it)

- `tiers-vs-rankings.md` — tier construction, cliffs, the operational
  rules tiers buy you.
- `adp-as-value.md` — ADP definition, the VONA reach test, fallen-player
  window, format-mismatch failure modes.
- `roster-construction.md` — positional timing evidence for RB, WR, QB,
  TE, K, and DEF in this 10-team PPR format.
- `positional-runs.md` — run signals, the tier-cliff break rule, the
  overpayment tax, the mirror-image (fear-reach) error.
- `handcuffs-and-byes.md` — the two-condition handcuff checklist, the
  bye-week onesie rule, the Bye Week Scarcity Meter.
- `slot-plans.md` — early/middle/late seat value and planning shape.
- `on-the-clock-procedure.md` — the full 60-90-second sequence, what the
  output must say, and what it must never do.
- `output-schema.json` — the extension UI's schema for the `draft`
  artifact. Write the artifact to match it exactly (see the agent
  prompt's Output section).
- `report.md` — the full R9 research report, every finding with its
  full context. Load for a hard case, or to revise this skill.
