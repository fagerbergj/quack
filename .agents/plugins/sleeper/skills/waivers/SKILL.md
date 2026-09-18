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

Source: `research-r2-waivers.md` (R2's own researched scope is a 10-team
PPR league running rolling-priority waivers AND $100 FAAB together — see
[Sleeper Support](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs)
on that combination). **Do not assume any specific league runs both:**
always check `sleeper_league`'s `waiver_type` and load exactly the
resource it maps to below — a league can be pure rolling, pure
reverse-standings, pure FAAB, or (per R2) a hybrid of rolling and FAAB.

## Which resource applies to which waiver type

`sleeper_league`'s `waiver_type` field already resolves Sleeper's
underlying `settings.waiver_type` integer (0/1/2) to one of these strings
(`tools_league.go`'s `waiverTypeName`) — match on the string it returns,
not the raw integer:

| `waiver_type` (from `sleeper_league`) | Underlying Sleeper int | Load |
| --- | --- | --- |
| `rolling` | 0 | `rolling-priority.md` |
| `reverse_standings` | 1 | `rolling-priority.md` (same "spend the queue slot" logic; priority resets weekly instead of on claim) |
| `faab` | 2 | `faab-bidding.md` |

If the league mixes FAAB alongside rolling/reverse-standings priority (as
R2's researched league does), load both `rolling-priority.md` and
`faab-bidding.md` and classify each candidate by channel first (below)
before sizing anything — but confirm this from the actual league data, not
by default.

## Decision procedure

1. **Classify the channel.** Every candidate add is a priority-queue claim
   or a FAAB bid (or, only if the actual league mixes both, either) —
   decide which before sizing. `sleeper_league` gives
   `waiver_type`/`waiver_budget`; `sleeper_roster` gives this team's
   `waiver_position`/`faab_left`.
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

Bid bands by need tier, handcuff screening/payoff rates, and K/DEF
cost/ownership gates are not restated here — see `faab-bidding.md`,
`handcuffs-and-byes.md`, and `streaming-k-def.md` below, loaded only when
the case calls for them.

## No published number

No fixed FAAB-by-week-of-season model is agreed — the report names four
incompatible published stances (aggressive early / mid-season concentration
/ small reserve / midpoint hold), plus a fifth that rejects fixed ranges
entirely in favor of a league-specific weekly Management Budget priced off
the league's own historical winning bids; state which stance is being
applied and why rather than presenting one as consensus. No published
heuristic is keyed specifically to 10-team leagues beyond the
auction-theory bid-shading math (~90% of true value at 10 bidders) — a
theoretical anchor, not a mainstream published rule.

## What bad advice looks like (judge-checkable)

Every recommended action names its channel (queue vs. bid) and its resource
cost; bid figures are expressed against *remaining* FAAB and weeks left, not
the original $100; K/DEF never gets more than $1-2; a drop cites a role
change, not a bad week; a trending/ownership number always carries its
platform, timestamp, and window and is never the sole reason to add.

## Resources (load only when the case calls for it)

- `output-schema.json` — the extension UI's schema for the `waivers`
  artifact. Write the artifact to match it exactly (see the agent prompt's
  Output section for the required-field walkthrough).
- `faab-bidding.md` — bid bands by need tier, the "three Ps" pre-bid gate,
  auction mechanics. **FAAB leagues only** (including the FAAB half of a
  league that mixes FAAB with rolling/reverse-standings priority).
- `rolling-priority.md` — queue mechanics, the aggressive-vs-patient
  tradeoff, the Sunday Night Shop tactic. Load for the priority-queue half
  of a mixed league, or the whole decision in a pure rolling or
  reverse-standings league.
- `streaming-k-def.md` — weekly K/DEF streaming criteria and cost rules.
- `handcuffs-and-byes.md` — the two handcuff screening questions and
  bye-week lead-time/double-stack rules.
- `report.md` — the full R2 research report (92KB). Load for a hard case,
  or to revise this skill.
