# Points against, close games, and luck framing

Source: `report.md` (R4), section 1.6 and disagreements (e), (f), (k).

## Pythagorean overshoot/undershoot is a population tendency, not a guarantee

Always express the gap in wins: "Pythagorean expectation: 5.9 wins" vs. the
actual record. The 10 largest overperformance gaps 1989-2015: **7 of 10
declined, none improved, average drop 3.4 wins**; teams 2-3 wins above
Pythagorean expectation declined by an average of **3 wins** the following
season (Bill Barnwell, ESPN, Aug 1, 2017). Counterpoint: betting on
early-season Pythagorean outliers hit only **~52% since 1999** — this is a
population regression tendency, not an individual guarantee (Andrew Healy,
Sep 2014). Use the population framing, and hedge with the betting number
when a single team/manager is the subject.

## One-score records are mostly luck once you know the margin

[NFL Analytics, "One-Score Records Are Luck" (Aug 9, 2026, 6,967 games
1999-2025, 829 franchise season-pairs, bootstrap CIs)](https://nflanalytic.com/explainer-close-game-luck.html):
a team's one-score win% correlates only **+0.08 (±0.04)** with its own
one-score record a year later, while point margin persists at **+0.40
(±0.03)**; the 140 team-seasons that won 70%+ of close games came back at
.515 the next year; controlling for point margin, close-game record adds
nothing. **"A receipt for luck already spent."** Lead any close-game claim
with the point margin, not the win-loss record in close games. Per-team
detection floor: with only ~8 close games a season, "a 3-game 0-3 close
record is indistinguishable from skill" — do not build a luck narrative on
fewer than about 8 close games.

## The league-chat luck-gap method

[ffwrapped, "Schedule Luck" (undated, retrieved 2026-09-18)](https://blog.ffwrapped.com/posts/unluckiest-team):
compute **all-play record** (each week, how many of the other 9 teams this
score would have beaten) → **expected wins** = all-play win% × games played
→ **luck gap** = expected wins − actual wins, explained with points against
and close losses counted at **<5 and <10 points**. Concrete beats vague:
"you missed the playoffs because of three losses by a combined 8.6 points"
is the target, not "unlucky by 2.4 expected wins."

## Points against is a weak, opponent-dependent signal

No published number cleanly splits PA variance into luck vs. skill — a
gap. Raw fantasy points-allowed correlates **under 0.10 (often under 0.06)**
with a position's next-week output at RB/WR/TE; points-for runs 0.15-0.20; a
combined cross-validated model reaches ~0.35, near Vegas-line territory
(Subvertadown, May 22 2022, updated Sep 10, 2026). Usable only after
opponent adjustment and only as a small modifier — a defense that has faced
four straight pass-heavy offenses will show inflated fantasy points allowed
that doesn't reflect true weakness (Matchup Analytics, live 2026 page).

## Usage over outcomes — the anti-luck rule for player-facing calls

[Fantasy Projection Lab (undated)](https://fantasyprojectionlab.com/regression-to-mean-in-fantasy/)
sample-size boundaries: **under 3 games** — "almost any rate stat should be
treated as noise"; **3-6 games** — directional only, regression weight
should still dominate; **7+ games** — beginning to carry meaningful
predictive weight. Don't call a player "due": "prior extreme games do not
increase the probability of a bounce-back; they are simply evidence of
variance" (Matchup Analytics). A role change ("target share climbs from 14%
to 23% following a team injury") is not the same claim as a hot streak —
name which one the evidence supports.

## Prefer "variance" / "regression" to "luck" in output

[NBC Sports fantasy](https://www.nbcsports.com/fantasy/football/news/regression-files-week-14-dont-give-up-on-rome-odunze)
deliberately avoids the word "luck" ("so crass and unsophisticated") in its
regression column; use "variance" or "regression" as the working vocabulary
in this agent's own copy.
