# Practice-report semantics

Source: `research-r1-startsit.md` §1.2-1.3, §1.8.

## Codes

- **DNP** = did not participate. **LP** = limited (<100% of normal reps).
  **FP** = full participation (100%) ([NFL.com, Aug 21, 2016](https://www.nfl.com/news/competition-committee-approves-revisions-to-injury-report-0ap3000000688693)).
- Game-status designations are **Questionable** ("uncertain as to whether the
  player will play"), **Doubtful** ("unlikely the player will participate"),
  **Out** ("will not play"). "Probable" was removed in 2016 because ~95% of
  Probable players played anyway ([ESPN, Aug 21-22, 2016](https://www.espn.com/nfl/story/_/id/17361111/nfl-streamlines-injured-players-game-status-removes-probable-designation)).
  None of these carry an official numeric probability.
- **A participation line is not itself an availability signal.** The policy
  requires listing any noteworthy injury even for a player certain to play,
  so a Full participant can carry a report line all season.
- Sleeper "mirrors the official NFL injury designations as closely as
  possible"; its "Out" is a game-week status that clears on the NFL week
  rollover unless re-issued ([Sleeper Support, updated Jun 4, 2026](https://support.sleeper.com/en/articles/3570017-injury-statuses-and-ir-eligibility)).
  There is no platform "day-to-day" field — that's commentary shorthand.

## Final (Friday) practice → chance to suit up, by injury type

2017-2023, 2,000+ injuries ([Footballguys, 09/01/2024](https://www.footballguys.com/article/2024-injury-index-chance-to-play-practice-participation)):

| Final Friday practice | All injuries | Concussion | Knee | Hamstring |
| --- | --- | --- | --- | --- |
| FP | 86% | 85% | 92% | 73% |
| LP | 71% | 57% | 81% | 62% |
| DNP | 29% | 29% | 28% | 20% |

Concussion and hamstring LP run well below the all-injury LP average (57%
and 62% vs. 71%) — stratify by injury type rather than using one universal
LP number. A DNP is a lean-out signal, not an automatic bench: roughly 1 in
3 DNP players still suit up.

## No published sequence rates

No league-wide play rate exists for a Wednesday→Thursday→Friday sequence
(e.g. "DNP-LP-LP"). Pattern rules circulating online ("DNP-DNP-DNP = out in
all but name") are editorial heuristics from secondary sites, not data — say
so rather than presenting them as measured probabilities. There is also no
published statistic for P(≥80% snap share | active + LP Friday) — a real
gap, not an oversight to paper over.

## Reporting calendar and the Thursday problem

| Game day | Practice reports due (1pm PT/day) | Final Game Status Report |
| --- | --- | --- |
| Sunday (incl. London) | Wed, Thu, Fri | Friday, 4pm ET |
| Monday | Thu, Fri, Sat | Saturday, 1pm PT |
| Thursday | Mon, Tue, Wed | Wednesday, 1pm PT |

([Chargers.com 2026-27 dates](https://www.chargers.com/news/national-football-league-important-dates-2026-2027); mirrored at [PFWA](https://www.profootballwriters.org/nfl-calendar/).)

A Thursday game has **no Thursday, Friday, or Saturday official report** —
the decision must be made by Wednesday 4pm ET. After that, the only channel
is the inactive list, filed ~90 minutes before kickoff ([WagerBird](https://wagerbird.com/learn/nfl-injuries-and-inactives)).
A team is not required to downgrade a Questionable before that list even if
it already knows he won't play — the "game-time decision loophole" ([NBC
Sports/ProFootballTalk](https://www.nbcsports.com/nfl/profootballtalk/rumor-mill/news/game-time-decision-loophole-helps-teams-avoid-having-to-downgrade-players-from-questionable)).
Flag a Thursday Questionable as a genuine coin-flip-class risk that cannot
be hedged after Wednesday.
