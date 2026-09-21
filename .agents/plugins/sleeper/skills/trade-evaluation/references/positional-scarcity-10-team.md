# Positional scarcity in this 10-team league

Source: `report.md` (R3 v2) section 1.1 "Positional scarcity in this league",
plus one supplemental formula from `research-r3-trades.md` (R3 v1), both
flagged where they are derived rather than published outright.

## Weekly NFL supply and this league's start rate

[Footballguys, Adam Harstad (Aug 27, 2020)](https://www.footballguys.com/article/2020-positional-scarcity):
weekly NFL supply is roughly **32 QB, 40 RB, 80 WR, 40 TE**; most leagues
start "60-70% of available running backs, but under 50% of available players
at every other position."

Applying that supply to this league's 10 teams (derived, not published for
this exact format): **10 QB / 32 (~31%)**, **20 RB / 40 (50%) before FLEX**,
20 WR / 80 (25%), 10 TE / 40 (25%). RB is still the scarcest starter
position here, though less extreme than a 12-team league's 24 RBs / 60%
start rate.

## The 10-team-specific expert read

[PFF, Nathan Jahnke (Aug 17, 2026)](https://www.pff.com/news/fantasy-football-2026-perfect-draft-strategy-round-by-round-for-10-team-leagues),
exact format match (10-team, 1QB, PPR redraft): "Roughly 12 or 13 of the
first 22 picks should be running backs" — only ~12-13 viable RBs in this
league size, versus 14-18 typically viable in a 12-team league. The same
piece: "a 10-team league has room for three set-and-forget wide receivers."

[Draft Sharks, Matt Schauf (Sept 2, 2026)](https://www.draftsharks.com/article/fantasy-football-draft-strategy-guide/10-team-ppr):
a 10-team league lets "every manager start out with a team he/she likes," so
managers "can afford to go after a top QB and/or TE" and "chase ultimate
upside" — this format favors opportunity volume (caveat: that guide assumes
a 3-WR lineup; this league runs 2-WR+FLEX, so treat directionally).

## Supplemental: a league-size replacement formula (R3 v1)

[Footballguys, Adam Harstad, "Calculating New Positional Baselines" (2015-08-04,
pre-2024 — the only published league-size generalization formula, still
used as the method)](https://www.footballguys.com/article/HarstadGeneralizingBaselines?article=HarstadGeneralizingBaselines):
for N teams, S weekly starters, F flex spots (PPR):
`QB = N×(S+0.75×SF)×1.56; RB = N×(S+0.3×F)×1.22; WR = N×(S+0.7×F)×1.22;
TE = N×S×1.81`. Applied to this league's 10-team 1QB/2RB/2WR/1TE/1FLEX
format, the article's own worked method yields a worst weekly starter around
**RB28, WR33, QB16, TE18** — a 10-team replacement-level RB sits well above
the 12-team-measured RB34.

The same source: with a FLEX in play, "the running back baseline was very
similar to the wide receiver baseline" (12-team measured: RB 8.3 vs WR 8.8
ppg) — the FLEX pulls RB and WR replacement value toward each other, so raw
RB scarcity in a 1-FLEX format is smaller than the position-count math alone
suggests. Do not state RB is scarcer than WR at the margin without this
caveat.

## What this means for trade pricing

Elite RB value runs above 12-team charts and mid-tier RB below them (deeper
10-team waiver wire); WR is the deepest, most liquid pool; TE replacement
near TE12 in PPR means mid-tier TEs are close to replacement and only elite
TEs are trade-worthy ([LineupLab, Sept 3, 2026](https://www.lineuplab.ai/guides/value-over-replacement-fantasy-football));
QBs are the least scarce position despite high points, since the league
starts under a third of the weekly QB supply
([SportsMonkie](https://sportsmonkie.com/tools/fantasy-football-trade-calculator/)).
