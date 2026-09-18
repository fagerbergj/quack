---
name: waivers
description: >
  Waiver-wire adds/drops and bid sizing before the weekly run: classifying a
  candidate, sizing a FAAB bid or spending rolling priority, reading
  trending-add/ownership data, handcuffs, streaming K/DEF, and bye-week
  coverage. Load whenever the job is "who should I add/drop this week" or
  sizing a waiver claim.
---

# Waiver targets

Source: `research-r2-waivers.md` (10-team PPR league, **this league runs
hybrid waivers: rolling priority AND $100 FAAB** — [Sleeper Support](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs)
documents this combination directly). Check `sleeper_league`'s
`waiver_type` for the actual league before applying FAAB math — a
rolling-only or reverse-standings league needs `rolling-priority.md`
instead of `faab-bidding.md`.

## Which resource applies to which waiver type

| `waiver_type` (from `sleeper_league`) | Load |
| --- | --- |
| `faab` | `faab-bidding.md` |
| `rolling` | `rolling-priority.md` |
| `reverse_standings` | `rolling-priority.md` (same "spend the queue slot" logic; priority resets weekly instead of on claim) |

The owner's own league is hybrid rolling + FAAB — read `rolling-priority.md`
for the claim-order half and `faab-bidding.md` for the dollar half; classify
every candidate by channel first (§ below) before sizing anything.

## Decision procedure

1. **Classify the channel.** Every candidate add is a priority-queue claim
   or a FAAB bid (or both, in this hybrid league) — decide which before
   sizing. `sleeper_league` gives `waiver_type`/`waiver_budget`;
   `sleeper_roster` gives this team's `waiver_position`/`faab_left`.
2. **Bucket the candidate.** Replacement starter (permanent role), handcuff
   (value only if a specific starter is hurt), streamer (1-2 weeks, matchup
   only), or lottery ticket (needs a second event to matter). The bucket
   drives the resource rule — see the thresholds table.
3. **Model add-minus-drop, not the add alone.** Name a drop for every add.
   Drop on a role change (snap/target share actually fell), never on one bad
   box score with the role intact.
4. **Interpret trending/ownership as scarcity, not value.** `sleeper_trends`
   and `sleeper_free_agents`' `trending_adds`/`owned_pct` tell you who else
   is bidding, not whether the player is good — that case rests on role and
   usage. A trending spike right after a box score is usually the scoreboard,
   not new information.
5. **K/DEF:** stream on matchup per week; see `streaming-k-def.md` — this is
   the one category where the rule doesn't depend on FAAB vs. rolling.
6. **Byes:** see `handcuffs-and-byes.md` for lead time and the double-stack
   bye trap.
7. **Write the output:** ranked candidates, a named drop for every add,
   priority/bid sized to this league's actual waiver type, `why` citing
   role/usage evidence, not just an ownership number.

## Thresholds table (source-dated)

| Signal | Number | Source |
| --- | --- | --- |
| The 24-hour rule | drop >24h on roster → waivers; <24h → immediate free agent | [ESPN](https://support.espn.com/hc/en-us/articles/360000071352-Claim-a-Player-Off-Waivers); [Sleeper](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs) |
| Handcuff pre-injury stash | $0-2 / minimum bid | [NovaPredict, Jun 10, 2026](https://novapredict.com/blog/fantasy-football-handcuffs) |
| Handcuff post-injury (role confirmed) | 25-40% of remaining FAAB | [Calculator Collection, Aug 10, 2026](https://www.calculatorcollection.org/en/articles/faab-waiver-wire-strategy/) |
| Handcuff draft payoff rate (top-24 RB1 backups, 2020-2024) | only 23% delivered 2+ startable weeks; 59% flat misses | [FantasyDutchman, 2025](https://thefantasydutchman.com/drafting-handcuff-rbs-valuable-or-just-a-myth/) |
| K/DEF bid cap | "$1-2, no exceptions" | [4for4, Aug 28, 2026](https://www.4for4.com/2026/preseason/ultimate-guide-winning-waiver-wire-2026) |
| K/DEF streaming ownership gate | ≤40% (K, Yahoo) / <50% (DEF, ESPN) — sources conflict, surface both | [4for4](https://www.4for4.com/2025/w3/fantasy-football-kicker-streaming-week-3-no-need-prater-fantasy-points); [ESPN 2026](https://www.espn.com.au/fantasy/football/story/_/id/49798496/2026-fantasy-football-d-st-road-map-early-season) |
| DEF matchup-stream vs talent-chase (3-season avg) | 10.4 pts/gm (5 best matchups) vs 9.6 pts/gm (top-5 talent) | [ESPN 2026](https://www.espn.com.au/fantasy/football/story/_/id/49798496/2026-fantasy-football-d-st-road-map-early-season) |

## No published number

No fixed FAAB-by-week-of-season model is agreed — four incompatible
published stances exist (aggressive early / mid-season concentration /
midpoint hold / reject-fixed-ranges); state which stance is being applied
and why rather than presenting one as consensus. No published heuristic is
keyed specifically to 10-team leagues beyond the auction-theory bid-shading
math (~90% of true value at 10 bidders) — a theoretical anchor, not a
mainstream published rule.

## What bad advice looks like (judge-checkable)

Every recommended action names its channel (queue vs. bid) and its resource
cost; bid figures are expressed against *remaining* FAAB and weeks left, not
the original $100; K/DEF never gets more than $1-2; a drop cites a role
change, not a bad week; a trending/ownership number always carries its
platform, timestamp, and window and is never the sole reason to add.

## Resources (load only when the case calls for it)

- `faab-bidding.md` — bid bands by need tier, the "three Ps" pre-bid gate,
  auction mechanics. **FAAB leagues only** — including this league's hybrid
  FAAB half.
- `rolling-priority.md` — queue mechanics, the aggressive-vs-patient
  tradeoff, the Sunday Night Shop tactic. Load for the priority-queue half
  of any claim in this league, or the whole decision in a pure rolling
  league.
- `streaming-k-def.md` — weekly K/DEF streaming criteria and cost rules.
- `handcuffs-and-byes.md` — the two handcuff screening questions and
  bye-week lead-time/double-stack rules.
- `report.md` — the full R2 research report (92KB). Load for a hard case,
  or to revise this skill.
