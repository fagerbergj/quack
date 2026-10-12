# Common false signals

Source: `research-r5-injury-news.md`, "What bad advice looks like."

## Team-published depth charts

Teams themselves label these charts "unofficial" ([Falcons.com, 09/15/2026](https://www.atlantafalcons.com/news/atlanta-falcons-depth-chart-carolina-panthers)).
Documented same-week divergence: a chart had one QB atop it on Tuesday, but
practice reps split first-team snaps with a different QB by Wednesday
([AJC, 09/16/2026](https://www.ajc.com/sports/2026/09/atlanta-falcons-qb-michael-penix-jr-says-he-may-be-ready-against-panthers/)).
Prefer practice reps (first-team snap allocation) over the published chart
whenever they're available and disagree; treat the chart as "basically
nothing actionable" on its own ([Fantasy Life](https://www.fantasylife.com/articles/newsletters/why-you-shouldnt-trust-depth-charts)).

## Armchair video diagnosis

Documented case: a self-styled "doctor" account diagnosed a player with a
high-ankle sprain from video; the player's own family corrected the
diagnosis (imaging showed a medial sprain), and the account still insisted
([Awful Announcing, 10/11/2024](https://awfulannouncing.com/nfl/christian-watson-social-media-injury-expert.html)).
Video-based diagnosis from a social account is a false signal, not a
source — prefer the team's own reporting or a calibrated model (e.g.
FantasyPros' "Are They Playing?") over a guess from footage.

## Participation level ≠ diagnosis, and ≠ guarantee of intent

"Limited participation" is defined purely as "less than 100% of normal
reps" — it carries no diagnostic content on its own ([2016 Injury Report
Policy](https://nyc3.digitaloceanspaces.com/sportsarchive-documents/prod/681949c029f45/06-07-16-2016-injury-report-policy.pdf)).
Even a Full-Participant listing doesn't guarantee the team's own honesty
about it: a team was fined $100,000 for listing a player as a full
participant when he had only worked with the scout team ([NFL.com,
10/31/2025](https://www.nfl.com/news/nfl-fines-ravens-100k-incorrectly-listing-lamar-jackson-week-8-injury-report)).
Read a participation level as a compliance filing, not a medical bulletin.

## A DEF transaction read as a league rule

A `sleeper_transactions` add/drop with a team code as the player id (e.g.
`CAR`, `BAL`) is a team defense (DEF) - a standard roster slot every league
streams weekly for matchups, not evidence of a custom mechanic. The tool
labels it `position: "DEF"`; a real league rule comes only from
`sleeper_league`'s settings, never inferred from transaction shapes.

## Stale injuries carried forward

An injury from a prior season doesn't automatically still apply — check the
current-week report rather than assuming a lingering limitation.
Documented case: a player limited early after returning from a prior
injury was full and removed from the report by Week 1 of the following
season ([Footballguys Week 1 injury report, 09/2026](https://www.footballguys.com/article/2026-nfl-week-1-injury-report-fantasy-football-impact-for-every-game)).
An answer that assumes an old designation still holds, without checking
this week's report, fails.
