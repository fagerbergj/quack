---
name: injury-and-news
description: >
  Reading and dating injury/practice-report changes and NFL news for fantasy
  impact: which reporters and aggregators are reliable, when news lands
  through the week, how to weigh conflicting reports, and common false
  signals (depth charts, armchair diagnoses). Load whenever the job is
  tracking what changed for a player or league this week.
---

# Injury and news reliability

Source: `research-r5-injury-news.md`. This skill covers *interpreting*
injury/news signals; `start-sit`'s `practice-report-semantics.md` covers the
practice-report play-rate tables themselves — load both when a start/sit
call turns on breaking news.

## Decision procedure

1. **Rank the source, not the claim.** Direct practice observation by a
   credentialed reporter > locker-room/one-on-one quotes > second-hand
   press-box accounts > anonymous "sources." The team's own announcement is
   "definitive but arrives last"; aggregators are the slowest useful tier
   ([Fantasy News Authority, flagged undated](https://fantasynewsauthority.com/beat-reporters-fantasy-news-value/)).
   Load `sources.md` for named-source guidance and its limits.
2. **Date and attribute every claim.** Name the reporter/outlet and state
   whether it's an observation or speculation. Never cite an aggregator
   without the original source underneath it.
3. **Weigh conflicting reports** by the source ranking above, not by
   recency or follower count. When two same-tier sources conflict, say so
   rather than picking one silently.
4. **Check the timing window.** A designation resolves on a fixed weekly
   calendar; see `news-timing.md` for the practice-report deadlines and the
   90-minute inactive-list rule — the only definitive game-day document.
5. **Screen for false signals** before acting: a team-published depth chart
   ("unofficial" by the team's own label), an armchair video diagnosis, or a
   participation level mistaken for a diagnosis. See `false-signals.md`.
6. **Write the trends artifact:** every item dated and sourced (a Sleeper
   field or a URL); Sleeper's own fields win on conflict with web news, per
   the agent's prompt.

## Thresholds table (source-dated)

| Signal | Number | Source |
| --- | --- | --- |
| Final-report Questionable → played (post-2016) | 70.7% (skill positions) | [Footballguys, 09/13/2025](https://www.footballguys.com/article/2025-chance-to-play-questionable-vs-doubtful) |
| Final-report Doubtful → played | 1%-25% depending on study — surface the range | [PFF, 06/14/2021](https://www.pff.com/news/nfl-pff-data-study-war-adjusted-injuries-lost); [Footballguys](https://www.footballguys.com/article/2025-chance-to-play-questionable-vs-doubtful); [Waiver Wizard, 07/06/2026](https://fantasywaiverwizard.com/learn/reading-injury-reports) |
| Inactive list filed before kickoff | 90 minutes (bylaw-verified) | [NFL.com, 05/21/2023](https://www.nfl.com/news/nfl-owners-pass-proposal-to-allow-teams-to-have-third-qb-active-on-game-days-wit) |
| "Probable" removed because it was uninformative | ~95% of Probable players played | [ESPN, Aug 21, 2016](https://www.espn.com/nfl/story/_/id/17361111/nfl-streamlines-injured-players-game-status-removes-probable-designation) |
| Post-ACL WR target share / YPRR drop (year after) | −17.9% target share, −26.6% yards/route | [Footballguys, 07/05/2025](https://www.footballguys.com/article/2025-digging-into-wrs-returning-from-acl-reconstruction) |
| IR minimum absence | 4 games | [PlaybookWire, 08/03/2026](https://playbookwire.com/nfl-injured-reserve-rules-how-ir-works-in-2026/) |

**Never state 75 minutes for the inactive-list deadline** — that figure
belongs to soccer/college officiating, not the NFL; the verified number is
90 (NFL Constitution Art. XVII §17.3).

## No published number

There is no league-level "GTD resolution rate" statistic — "game-time
decision" is media shorthand, not an official designation, and no source
publishes how often a GTD resolves either way. Reporter-lag figures
(beat reporters 10-30 min ahead of designations, aggregators 15-60 min
behind) are undated directional estimates, not measured data — present
them as such, not as precise numbers.

## What bad advice looks like (judge-checkable)

Treating "Questionable"/"expected to play" as guaranteed; treating
"Doubtful" as either a coin flip or certain-out with no plan; carrying last
season's injury into this week's status without checking the current
report; citing an aggregator with no named original source; trusting a
team depth chart as current when practice reps disagree; citing "75
minutes" for inactives; a FAAB recommendation for an injury replacement
with no dollar figure or % of remaining budget attached.

## Resources (load only when the case calls for it)

- `output-schema.json` — the extension UI's schema for the `trends`
  artifact. Write the artifact to match it exactly (see the agent prompt's
  Output section for the required-field walkthrough).
- `season-notes-schema.json` — the schema for the separate `season-notes`
  artifact (only written when you're appending a durable, league-specific
  note).
- `sources.md` — the reliability tier (beat reporter > aggregator) and its
  documented limits; load when a news item's *source*, not just its
  content, is in question.
- `news-timing.md` — the practice-report and inactive-list calendar, and
  why Week 1 is the least data-rich week of the season.
- `false-signals.md` — depth-chart unreliability, armchair-diagnosis
  cases, and the practice-participation-is-not-a-diagnosis trap.
- `report.md` — the full R5 research report (54KB). Load for a hard case,
  or to revise this skill.
