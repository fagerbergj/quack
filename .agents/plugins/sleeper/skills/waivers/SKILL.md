---
name: waivers
description: >
  Waiver-wire adds/drops and bid sizing before the weekly run: classifying a
  candidate, sizing a FAAB bid or spending rolling priority, reading every
  team's roster and priority in the league, checking current news on close
  calls, and covering handcuffs, streaming K/DEF, and byes. Load whenever
  the job is "who should I add/drop this week" or sizing a waiver claim.
---

# Waiver targets

Mechanics below are Sleeper's own documentation: [What types of waivers do
you support?](https://support.sleeper.com/en/articles/1876041-what-types-of-waivers-do-you-support),
[How do FAAB and waivers work?](https://support.sleeper.com/en/articles/9657110-how-do-faab-and-waivers-work),
[Waivers for Regular Season & Playoffs](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs),
and [What is the 24-Hour Rule?](https://support.sleeper.com/en/articles/4519433-what-is-the-24-hour-rule).
Bid bands, screening rules, and strategy framing come from `research-r2-waivers.md` (R2), which Sleeper doesn't publish itself.

**A league's `waiver_type` is exactly one of three settings, never a
combination.** `sleeper_league` resolves Sleeper's single `settings.waiver_type` value to `rolling` (the default), `reverse_standings`, or `faab` — a league runs one of these, not "rolling AND FAAB" together. Sleeper's API always returns a `waiver_budget` setting, and `sleeper_roster` always returns `faab_used`/`faab_left`, regardless of which type is active — those fields are only meaningful, and only ever spent, when `waiver_type` is `"faab"`. **A rolling or reverse-standings league that reports `waiver_budget: 100` is not running FAAB — ignore that field and size every claim in priority-queue terms.** Even a pure FAAB league still tracks a `waiver_position` number, but only as the tiebreaker when two bids match, not as a resource anyone spends. Always check `sleeper_league`'s `waiver_type` first and load exactly the resource it maps to below — never assume a hybrid.

## Which resource applies to which waiver type

`sleeper_league`'s `waiver_type` field already resolves Sleeper's underlying `settings.waiver_type` integer (0/1/2) to one of these strings (`tools_league.go`'s `waiverTypeName`) — match on the string it returns, not the raw integer:

| `waiver_type` (from `sleeper_league`) | Underlying Sleeper int | Load |
| --- | --- | --- |
| `rolling` | 0 | `rolling-priority.md` |
| `reverse_standings` | 1 | `rolling-priority.md` (same "spend the queue slot" logic; priority resets weekly instead of on claim) |
| `faab` | 2 | `faab-bidding.md` |

## Ordered claims sharing a drop are normal, never an error

A manager can submit as many ranked claims as they like, each naming its own conditional drop, and Sleeper processes them in the order ranked
([Sleeper Support](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs)).
Claim A (add X, drop Z) followed by claim B (add Y, drop Z) is a manager expressing an ordered fallback: if claim A succeeds, Z is already gone and claim B's identical drop simply can't fire, so claim B is skipped for that reason and nothing else. **Never describe multiple pending claims that name the same drop as a conflict, a mistake, or something to fix** — it is how priority waivers express "try this first, else that."

## Decision procedure

1. **Classify the channel.** `sleeper_league` gives `waiver_type` (and
   `waiver_budget`, meaningful only if that type is `faab`); `sleeper_roster`
   gives this team's `waiver_position`/`faab_left`.
2. **Bucket the candidate.** Replacement starter (permanent role), handcuff
   (value only if a specific starter is hurt), streamer (1-2 weeks, matchup
   only), or lottery ticket (needs a second event to matter). The bucket
   drives the resource rule — see the thresholds table.
3. **Model add-minus-drop, not the add alone.** Name a drop for every add.
   Drop on a role change (snap/target share actually fell), never on one bad
   box score with the role intact. Ranking several candidates against the
   same drop is a valid ordered-fallback list, not an error (see above).
   Before naming any drop, check it against this round's `sleeper_roster`
   `starters` list — never guess or recall a player's role from memory. A
   drop that is a current starter is still allowed, but say so explicitly
   (e.g. "drops your starting RB2") rather than labeling a starter as bench
   or cuttable depth.
4. **Interpret trending/ownership as scarcity, not this league's
   competition.** `sleeper_trends` and `sleeper_free_agents`'s
   `trending_adds`/`owned_pct` are platform-wide Sleeper numbers — activity
   and ownership across every Sleeper league, not this one. They tell you a
   player is popular somewhere, never that a specific team in *this* league
   is about to claim him, and never that the player is good on his own. That
   read comes from this league's actual rosters and priorities — step 5.
5. **Read every other roster before ranking anything.** Call
   `sleeper_roster()` for your own team, then `sleeper_standings()` to list
   every `roster_id` in the league, then `sleeper_roster(roster_id=...)` for
   every other team. For each top target, note which teams are thin at that
   position, where each sits in waiver priority relative to you
   (`waiver_position` — lower moves first in rolling/reverse-standings), and
   — only when `waiver_type` is `faab` — their remaining `faab_left`.
   Claim-now-vs-wait advice rests on who in this league can actually beat you
   to the player, never on the global trending count.
6. **Cover every widely-available player, not just the ones already in
   mind.** From `sleeper_free_agents`, any unrostered player with a high
   `owned_pct` (rostered in most other Sleeper leagues) or a real
   `trending_adds` count is a candidate to check, at every position — not
   only the position assumed to be thin. Before ranking or dismissing one,
   call `sleeper_player`: `sleeper_free_agents` carries no injury/practice
   field, so a highly-owned player still sitting available is either hurt or
   a miss, and only `sleeper_player`'s `injury_status`/`practice_description`
   says which. Dismiss an injured one with the reason stated; rank a healthy
   one.
7. **Check current news on every top claim and every drop.** Sleeper's own
   projections and injury tags update on a fixed schedule and lag practice
   reports and beat-writer news. For each top claim and each recommended
   drop, run `web_search`/`web_fetch` and cite what changes the call as an
   inline markdown link in that candidate's `why` — never let a close call
   ride on Sleeper's fields alone.
8. **K/DEF:** stream on matchup per week; see `streaming-k-def.md` — this is
   the one category where the rule doesn't depend on FAAB vs. rolling.
9. **Byes:** see `handcuffs-and-byes.md` for lead time and the double-stack
   bye trap.
10. **Write the output:** ranked candidates (rank = claim order — the
    sequence to submit them in), a named drop for every add (checked against
    the current starters, never mislabeled), priority/bid sized to this
    league's actual waiver type, `why` citing role/usage evidence and, for a
    close call, the news that tipped it. Set `waiver_type`, `my_priority`
    (your current `waiver_position`), and `teams` (the league size, from
    `sleeper_standings`) directly from the tool data you already pulled.
    List every widely-owned/trending player you checked and passed over in
    `also_checked` — `{player, why}`, one short sentence each — so coverage
    (step 6) is visible in the artifact, not just implied.

## Thresholds table (source-dated)

| Signal | Number | Source |
| --- | --- | --- |
| The 24-hour rule | a player added via free agency must be rostered 24h before being dropped, or the drop returns him straight to free agency, skipping waivers | [Sleeper Support](https://support.sleeper.com/en/articles/4519433-what-is-the-24-hour-rule); also [ESPN](https://support.espn.com/hc/en-us/articles/360000071352-Claim-a-Player-Off-Waivers) |
| Claims process in submission-rank order | "you can make as many claims as you like, and each claim is processed in the order you ranked it, with each needing a roster spot (or a conditional drop)" | [Sleeper Support](https://support.sleeper.com/en/articles/3978868-waivers-for-regular-season-playoffs) |

Bid bands by need tier, handcuff screening/payoff rates, and K/DEF cost/ownership gates are not restated here — see `faab-bidding.md`, `handcuffs-and-byes.md`, and `streaming-k-def.md` below, loaded only when the case calls for them.

## No published number

No fixed FAAB-by-week-of-season model is agreed — the report names four incompatible published stances (aggressive early / mid-season concentration / small reserve / midpoint hold), plus a fifth that rejects fixed ranges entirely in favor of a league-specific weekly Management Budget priced off the league's own historical winning bids; state which stance is being applied and why rather than presenting one as consensus. No published heuristic is keyed specifically to 10-team leagues beyond the auction-theory bid-shading math (~90% of true value at 10 bidders) — a theoretical anchor, not a mainstream published rule.

## What bad advice looks like (judge-checkable)

Sizing a rolling/reverse-standings league's claim in FAAB dollars because `waiver_budget` reported a non-zero number; describing a manager's own ordered claims that share a drop as a mistake or a conflict; ranking or dismissing a widely-owned or trending available player without a `sleeper_player` check; framing a global trending/ownership number as proof this league's competition will move fast, instead of reading any other roster; recommending a claim or a drop on a close call with no cited, actually-fetched current news; a bid figure expressed against the original $100 instead of remaining FAAB; a drop that cites a bad week instead of a role change; K/DEF costing more than $1-2; labeling a current starter "bench" or "depth" to justify dropping him instead of checking `sleeper_roster`'s `starters` list.

## Resources (load only when the case calls for it)

- `output-schema.json` — the extension UI's schema for the `waivers`
  artifact. Write the artifact to match it exactly (see the agent prompt's
  Output section for the required-field walkthrough). This copy doesn't yet
  list `waiver_type`/`my_priority`/`teams`/`also_checked` - the schema has
  no `additionalProperties: false`, so they validate today; a pin bump will
  refresh this copy once the upstream schema documents them.
- `faab-bidding.md` — bid bands by need tier, the "three Ps" pre-bid gate,
  auction mechanics. FAAB leagues only.
- `rolling-priority.md` — queue mechanics, the aggressive-vs-patient
  tradeoff, the Sunday Night Shop tactic. Rolling and reverse-standings
  leagues.
- `streaming-k-def.md` — weekly K/DEF streaming criteria and cost rules.
- `handcuffs-and-byes.md` — the two handcuff screening questions and
  bye-week lead-time/double-stack rules.
- `report.md` — the full R2 research report (92KB). Load for a hard case,
  or to revise this skill. Its own framing describes the researched league
  as running "rolling-priority waivers + $100 FAAB together" — that's not a
  real Sleeper setting (see above); use its bid bands and screening rules,
  not that framing.
