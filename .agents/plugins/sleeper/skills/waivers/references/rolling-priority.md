# Rolling-priority waivers

Source: `research-r2-waivers.md` §1.1, §2.1.

## Mechanics

Starts in inverse draft order; every successful claim drops the claiming team to the bottom of the queue while everyone else moves up one; the order
does **not** reset weekly ([Sleeper Support](https://support.sleeper.com/en/articles/1876041-what-types-of-waivers-do-you-support)).
Reverse-standings waivers use the same "spend the queue slot" logic, but priority resets weekly by current record instead of on claim.

`sleeper_league` still reports a `waiver_budget` and `sleeper_roster` still reports `faab_used`/`faab_left` for a rolling or reverse-standings league — Sleeper's API returns those fields regardless of `waiver_type`. Neither is spendable here; the only resource in play is the queue slot (`waiver_position`). Read every other team's `waiver_position` with `sleeper_roster(roster_id=...)` (roster ids come from `sleeper_standings`) to see who is ahead of or behind you for a shared target.

## The core tradeoff (both positions published, surface both)

- **Aggressive ("sniping is dead"):** burn priority on real value when it
  surfaces — the discipline lives in pre-set rules, not in waiting. "Sunday
  Night Shop": buy next week's target during this week's window or
  free-agency gap, before the market converges ([4for4, Aug 28, 2026](https://www.4for4.com/2026/preseason/ultimate-guide-winning-waiver-wire-2026)).
- **Patient (priority as ammo):** spending priority early on a medium-upside
  player means entering a later week at the back of the queue when a true
  RB1 vacancy opens — priority is an option with real value in reserve
  ([Fantasy Strategy Guide, flagged secondary](https://fantasystrategyguide.com/waiver-wire-strategy)).
- **Point both camps agree on:** never spend full priority on a streamer.

## Rules that apply regardless of stance

- The 24-hour rule gates every drop of a recently-added free agent: added
  and dropped within 24h → returns straight to free agency, skipping
  waivers entirely; held 24h or longer before the drop → goes through
  waivers on the normal queue ([Sleeper Support](https://support.sleeper.com/en/articles/4519433-what-is-the-24-hour-rule); also [ESPN](https://support.espn.com/hc/en-us/articles/360000071352-Claim-a-Player-Off-Waivers)).
- Claims process top-to-bottom in the order the manager ranked them, each
  against its own conditional drop — several claims naming the same drop is
  a normal ordered fallback, not an error (see SKILL.md).
- Sabotage-drop counter-signal: when a struggling team cuts a big name to
  fill a need, someone claims the name on reputation and drops someone
  genuinely useful to do it — that dropped player is often the real target
  ([4for4, Aug 28, 2026](https://www.4for4.com/2026/preseason/ultimate-guide-winning-waiver-wire-2026)).
