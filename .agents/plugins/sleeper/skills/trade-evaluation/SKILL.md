---
name: trade-evaluation
description: >
  Evaluating a Sleeper fantasy trade offer or scanning the league for trade
  candidates in a 10-team full-PPR redraft league: chart limits, positional
  scarcity, the 2-for-1 consolidation discount, roster fit, win-now vs.
  depth, playoff-position modifiers, and fairness/collusion norms. Load
  whenever the job is judging a specific trade offer or finding trade
  targets.
---

# Trade evaluation

Source: `report.md` (the R3 v2 research report), plus `research-r3-trades.md`
(R3 v1) for a small number of supplemental facts each reference marks
explicitly. Every number below is as published, with its source; where none
exists, say so instead of inventing one — see "No published number" below.
This league is 10-team, full-PPR, rolling waivers, 100 FAAB, QB/RB/RB/WR/WR/
TE/FLEX/K/DEF + 5 bench, 6-of-10 playoffs from week 15.

## Decision procedure

1. **State the league-size mismatch on any chart number.** Most public trade
   charts are calibrated to 12-team, 1QB, full-PPR. Quoting one for this
   10-team league without noting the mismatch is the single most common
   failure mode. Load `value-charts-and-limits.md` for what each chart
   family is actually grounded in (consensus rankings, real-trade market
   data, or explicit VOR) and pick one to name.
2. **Model each partner before pricing.** Read that team's roster
   (`sleeper_roster` with their `user`), check it against the league's
   starting slots (`sleeper_league`), and read its record and playoff
   position (`sleeper_standings`) to tell a contender from a rebuilder.
   That model - positional surplus and need, contender or rebuilder - is
   what decides which of your players they'd value and what they'd give up
   for it, not just chart parity. Load `roster-fit.md`'s mutual-need rule.
3. **Check outside news for every traded player.** Sleeper's own
   projections and injury/practice tags lag beat-writer reporting and
   practice-report news (see `sleeper:injury-and-news`). Search the open
   web for each traded player and cite what you find inline as a markdown
   link, especially when it would move the price.
4. **Price by scarcity, not raw points.** RB is the scarcest starter
   position in this league (~50% of weekly supply started, vs ~25% at
   WR/TE, ~31% at QB) but the FLEX narrows the RB/WR gap versus a
   no-FLEX league. Load `positional-scarcity-10-team.md` for the numbers
   and the 10-team-specific expert read.
5. **Discount a 2-for-1, and price the forced cut.** Packages do not sum at
   100%; a shallow 10-team start-9 justifies a larger-than-default
   second-asset discount. Load `consolidation-rules.md` for the published
   discount figures and when taking two is actually the right move.
6. **Weigh roster fit and win-now vs. depth.** A 5th RB on an already-deep
   roster earns no scarcity premium (the reversal rule); a contender inside
   the playoff cutline and a team past its window get different published
   advice. Load `roster-fit.md` for the fit band and the win-now-premium
   numbers.
7. **Check fairness and collusion norms before recommending a lopsided
   deal**, especially with an eliminated team or a playoff rival. Load
   `fairness-and-vetoes.md` — collusion has no single definition, and a
   one-sided-but-voluntary trade ("fleecing") is not collusion.
8. **Write the verdict and the note.** Land on exactly one of send / counter
   / decline (a finder run: a ranked, bounded list of at most three
   candidates), each in one or two short sentences that name the deciding
   line item and, for a suggestion, the partner's need it fills. Load
   `persuasive-writeup.md` for how to state the fairness band used, write
   the lineup before/after, and structure a counter that keeps the spirit
   of the original offer.

## No published number

Several commonly cited trade thresholds have no primary source for this
exact format: a single 10-team replacement-value chart, a fixed win-
probability differential by playoff seed, and a quantified counter-spread
threshold ("counter if within X%"). When a case turns on one of these, say
explicitly that no published number exists for this exact question and give
the qualitative reasoning instead — do not invent a figure to sound precise.

## What bad advice looks like (check your own answer against this)

Transplanting a 12-team chart number into this league without adjustment;
pricing by raw points instead of scarcity; summing a 2-for-1 at 100% with no
discount and no forced cut priced; overpaying a scarcity premium on an
already-stocked position; pricing a late-season deal at full-season value
while fighting for a bracket spot, or demanding full value from a team with
under 15-20% playoff odds and under 5 weeks left; trading on 2-4 weeks of
early-season data as if it were signal; recommending a deal that benefits a
third team's rival or tanks its playoff hopes; quoting a chart as the
verdict instead of a negotiation anchor; and counter-spamming with no named
deficiency. Each pattern is sourced in `report.md` section 3.

## Resources (load only when the case calls for it)

- `output-schema-trade.json` — the extension UI's schema for the `trade`
  artifact (a specific offer). The artifact carries exactly this
  schema's properties; read it before writing.
- `output-schema-trade-finder.json` — the schema for the `trade-finder`
  artifact (a league-wide scan). Same rule.
- `value-charts-and-limits.md` — what a chart's numbers are grounded in, why
  charts go stale, and the 12-team-vs-10-team mismatch. Load before citing
  any chart figure.
- `positional-scarcity-10-team.md` — the supply/start-rate numbers for this
  league size and the FLEX's dampening effect on RB scarcity. Load when
  scarcity is the deciding factor.
- `consolidation-rules.md` — the 2-for-1 discount figures and the forced-cut
  math. Load whenever the offer is multi-for-one on either side.
- `roster-fit.md` — the fit-adjustment band, the reversal rule, and win-now
  vs. depth with the win-now-premium numbers. Load when the call depends on
  standings or roster construction, not just player value.
- `fairness-and-vetoes.md` — collusion definitions, the fleecing distinction,
  and veto mechanics. Load whenever a suggested deal could look one-sided to
  the league, involves an eliminated team, or a partner asks about vetoing.
- `persuasive-writeup.md` — how to state a fairness band, write the lineup
  before/after, and structure an accept/counter/decline. Load when drafting
  the reply's persuasive case, not just the artifact.
- `report.md` — the full R3 v2 research report (73KB), every finding with
  its full context. Load for a hard case, or to revise this skill.
