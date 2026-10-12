# Value charts and their limits

Source: `report.md` (R3 v2), section 1.1 and 2.1/2.10, unless noted.

## Every published chart states a league size — check it

Nearly every mainstream trade-value chart is built for **12-team, 1QB, full-PPR**:
RSJ's 2026 Week 2 chart is explicitly "tailored to a 12-team, full-PPR, 1QB
league" ([Roto Street Journal, Sep 15, 2026](https://www.rotostreetjournal.com/2026-09-15/2026-fantasy-football-week-2-trade-value-chart/)),
and PFF's chart template "assumes a 12-team league and a starting lineup of
1 QB, 2 RB, 2 WR, 1 TE, and 1 Flex" ([PFF, Sept 6, 2017](https://www.pff.com/news/fantasy-football-trade-value-chart-what-are-the-fantasy-values-heading-into-week-1)).
This league is 10-team. Quoting a 12-team chart number without noting the
mismatch, and without adjusting for this league's scarcity, is the report's
named bad-advice pattern #1.

## What the numbers are actually grounded in (name which)

Three different families exist, and a chart rarely says which it is:

- **Consensus expert rankings / ROS projections** — FantasyPros, CBS
  ("expected future performance, future schedule and, most importantly,
  public sentiment. Past performance isn't a major factor" —
  [CBS, 2024-08](https://www.cbssports.com/fantasy/football/news/fantasy-football-2024-week-1-trade-chart-and-rest-of-season-rankings-help-you-win-now-before-the-games-begin/)),
  FanDuel/numberFire.
- **Completed real trades (market)** — [FantasyCalc](https://fantasycalc.com/about)
  (7,211,577 real trades, run through an optimization algorithm, updated
  multiple times a day; its API exposes `numTeams` as a parameter, so it is
  the only major source computing values per league size); Stats Guy's
  least-squares fit on real trades, which "takes a day or two of post-news
  trading before values fully adjust" ([Stats Guy](https://statsguyfantasy.com/methodology/how-values-work)).
- **Explicit VOR (value over replacement)** — `VOR = player projected points
  − replacement player projected points`; two sites can disagree without
  using different projections, only different replacement baselines
  ([LineupLab, Sept 3, 2026](https://www.lineuplab.ai/guides/value-over-replacement-fantasy-football)).

Charts mechanically shrink as the season ends because they are rest-of-season
tools ([RSJ, Sep 15, 2026](https://www.rotostreetjournal.com/2026-09-15/2026-fantasy-football-week-2-trade-value-chart/)).

## Charts are stale between updates

Weekly static charts (RSJ, CBS, FantasyPros) dominate actual use, but "values
move daily, charts update weekly" and one chart number hides a floor/median/
ceiling range ([NovaPredict, Jun 30, 2026](https://novapredict.com/blog/fantasy-football-trade-value-chart)).
Live market values update "multiple times per day"
([FantasyCalc](https://fantasycalc.com/)) or lag breaking news "a day or two"
([Stats Guy](https://statsguyfantasy.com/methodology/how-values-work)). A
chart is a negotiation anchor, not a verdict — RSJ itself: charts "are a
reference… not gospel," and asserting chart-parity as final "could shut the
door on negotiations real quick" ([RSJ, Sep 15, 2026](https://www.rotostreetjournal.com/2026-09-15/2026-fantasy-football-week-2-trade-value-chart/));
this is bad-advice pattern #9.

## Ranking on raw points ignores scarcity

Comparing players on PPG alone (an "efficient" WR offered for a top RB)
ignores that this league starts roughly 50% of the weekly RB supply vs 25%
at WR/TE (see `positional-scarcity-10-team.md`) — bad-advice pattern #2.

## No published number

No source publishes a replacement-level baseline or trade-value chart
specific to this league's exact format (10-team, 2-RB/2-WR+FLEX PPR); the
10-team picture in `positional-scarcity-10-team.md` is derived arithmetic,
not a published table — say so rather than quoting a derived number as if it
were a cited one.
