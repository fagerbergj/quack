---
name: matchup-recap
description: >
  Weekly league digests and single-team retros for a 10-team full-PPR
  Sleeper league: matchup-block structure, length norms, the closed-form
  playoff-line method, and how to tell real luck from noise in close games
  and points against. Load whenever the job is "how did the league do this
  week" or a retro on one team's own week.
---

# Weekly digest and retro

Source: `report.md` (the R4 research report). Every number below is as
published, with its source; where none exists, say so instead of inventing
one — see `keep-it-short.md`'s "Named gaps" section.

## Which job you're on

A **digest** covers every game in the league for one week, before or after
kickoff. A **retro** covers one team's own week: what was started versus
what the roster could have scored.

## Decision procedure

1. **Use the standard matchup-block shape.** One repeating block per game,
   same field order every week; state the league format ("10 teams, full
   PPR, 6 playoff spots") whenever it affects a claim. Load
   `recap-structure.md` for the field order and the usage-over-points rule
   for any forward-looking call.
2. **Verify every score and every lineup-legality claim against tool data.**
   Never infer game flow ("comeback") from a final score alone; for a
   retro's "should have started X," confirm X was actually eligible for an
   open slot. See `recap-structure.md`.
3. **If publishing a playoff line or odds figure, use the closed-form
   method and state it.** This agent has no simulator — Pythagorean
   expectation, regression toward .500, and the 6th-largest-projected-win-
   total cutoff, per `playoff-odds-from-records.md`. Never publish a
   probability with no stated method.
4. **Separate real luck from noise before calling anything "lucky."** A
   close-game record is mostly noise once point margin is known; don't
   build a luck narrative on fewer than ~8 close games. Load
   `points-against-and-luck.md` for the correlation numbers and the
   all-play luck-gap method.
5. **Keep it short and concrete.** A league-chat digest runs ~200-500 words
   with depth on 2-3 stories, not six filled-out sections; a retro's
   `summary` names the single biggest swing. Load `keep-it-short.md` for the
   length rules and the failure patterns (filler, fake precision, hedging,
   stat dumps) to check your own draft against.

## Writing the digest

Cover every game in the league, not just the user's own matchup. Every
number traces to a `sleeper_matchup` or `sleeper_standings` call in this
session. Close with a one-line `summary` naming the week's real story (the
margin that mattered, the high score, the blowout) — never a bare scoreline
recital.

## Writing the retro

Compare what was started to what the roster could have scored, slot by
slot. Grade each start/sit call against what actually happened, and
separate a bad process (ignored evidence available at the time) from bad
luck (a reasonable call that didn't pay off) — see
`points-against-and-luck.md` for the variance-vs-skill distinction that
separation depends on.

## Resources (load only when the case calls for it)

- `output-schema-digest.json` — the extension UI's schema for the `digest`
  artifact. Required top-level: `week`, `past`, `games`. Write the artifact
  to match it exactly.
- `output-schema-retro.json` — the schema for the `retro` artifact. Required
  top-level: `week`, `started`, `best`, `left`.
- `recap-structure.md` — the matchup-block field order, length-norm
  citations by element, and the usage-over-points rule for forward calls.
  Load for any digest or preview write-up.
- `playoff-odds-from-records.md` — the closed-form Pythagorean/regression
  method for a playoff line or probability, with the one dated fantasy
  exponent anchor. Load whenever the job touches playoff odds or the
  playoff line.
- `points-against-and-luck.md` — which "luck" claims are backed by data
  (Pythagorean overshoot, the luck-gap method) and which are not (raw close-
  game records, raw points against). Load whenever a digest or retro calls
  a result lucky, unlucky, or due for regression.
- `keep-it-short.md` — the length rules and the concrete failure patterns
  (filler, fake precision, hedging, stat dumps) to self-check a draft
  against before writing the artifact.
- `report.md` — the full R4 research report (71KB), every finding with its
  full context. Load for a hard case, or to revise this skill.
