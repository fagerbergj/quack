# League fairness, collusion, and vetoes

Source: `report.md` (R3 v2) section 1.5, 2.7, 2.8, plus two supplemental
Sleeper-mechanics facts from `research-r3-trades.md` (R3 v1) marked below.

## Collusion has no single definition — name which test applies

Platform policies use different tests for the same word:

- **Outcome-based (ESPN):** "When one team makes moves to benefit another
  team without trying to improve its own position" — no secrecy or side
  agreement required. Named examples: one-sided trades, dropping a player so
  a partner can claim it. Standard League members are "expected to
  veto/protest all unfair/collusive trades"; **4 votes** are required to
  deny a trade ([ESPN Fair Play/Conduct policy](https://support.espn.com/hc/en-us/articles/115003903111-Fair-Play-Conduct-Collusive-Transaction)).
- **Intent-based (FSG):** "was this trade made with genuine intent to
  improve the manager's competitive standing?" — the informal review
  threshold is the "8-of-10 rule": if 8 of 10 experienced managers would
  reject the trade, it warrants review ([FantasyStrategyGuide, undated](https://fantasystrategyguide.com/collusion-and-ethics-in-fantasy/)).
- **Agreement-based (R3 v1, [NFL.com policy](https://support.nfl.com/hc/en-us/articles/4989074619804-Fair-Play-and-User-Conduct-Policy)):**
  collusion requires an exchange of value *outside* the trade itself — side
  deals, cash, a trade-back promise. Dropping most of a roster near-
  simultaneously is separately prohibited.

## Fleecing is not collusion

A one-sided but voluntary trade between honest parties stays legal:
"fairness isn't the same as legality" — the "that's what you get" precedent
([ESPN Commissioner Court, Randy Scott, Nov 13, 2015](https://www.espn.com/fantasy/football/story/_/id/14119741/collusion-quitters-more-fantasy-football));
[RandomDraftOrder, Ryan Glab (Nov 25, 2025)](https://www.randomdraftorder.com/articles/fantasy-football-disputes/):
"a bad trade is not the same as collusion. It's an owner making a
miscalculation." The named red flag that *is* worth scrutiny: "teams
mathematically eliminated trading away best assets" for injuries or bench
fillers to a contender (same source).

## Canonical collusion case

[PFF reprint, *Steel Curtain v. Rusty Trombones* (Nov 12, 2011)](https://www.pff.com/news/fantasy-fantasy-judgment-decision-in-fantasy-football-collusion-case):
a top-waiver-priority team claimed a player solely to trade it to a partner
who couldn't claim it directly. The trade itself was fair — the prearranged
waiver-order exploitation was the collusion. Remedy: the player was frozen
while held, and the two teams were barred from trading with each other for
the rest of the season.

## Veto vs. commissioner-only review — both are live norms

- **League-wide veto:** [ESPN rules page (2009-03-10)](https://www.espn.com.sg/fantasy/football/ffl/story?page=fflrulestradesstandard2009):
  the classic 10-team threshold is **4 of 10 managers voting to veto within
  48 hours**.
- **Commissioner-only, skeptical of votes:** [RandomDraftOrder (Nov 25,
  2025)](https://www.randomdraftorder.com/articles/fantasy-trades-vetoes-collusion/):
  a league-wide vote is "tyranny of the majority," where bubble teams veto
  rivals' deals out of self-interest.

Sleeper supports both models and the commissioner retains ultimate control
at any time, including reversing an already-completed trade ([Sleeper
Support, Jul 9, 2022](https://support.sleeper.com/en/articles/3200544-can-i-veto-a-trade);
[Sleeper Support, Mar 19, 2024](https://support.sleeper.com/en/articles/3188802-how-to-trade)).
There is no platform kill switch for trading; the sanctioned workaround is
an early deadline plus a bounded review window plus vetoes ([Sleeper
Support, Mar 20, 2024](https://support.sleeper.com/en/articles/4258022-can-i-disable-trading)).
Read this league's own settings for its deadline and review-window values —
Sleeper has no platform default.

## Supplemental Sleeper mechanics (R3 v1)

- **24-hour rule:** a free-agent add must be held 24 hours before it can be
  dropped, or it returns to free agency — the platform's structural
  anti-waiver-churn mechanic ([Sleeper Support](https://support.sleeper.com/en/articles/4519433-what-is-the-24-hour-rule)).
- **Weekend lock:** a player who has already played that week stays locked
  to the team that started them for that week's scoring, so an accepted
  trade's timing near game day can leave one side holding both players'
  output for that week ([Sleeper Support, 2024-03-20](https://support.sleeper.com/en/articles/3351190-how-could-a-weekend-trade-impact-my-team)).

## Do not invent platform mechanics (R3 v1 guardrail)

No platform (Sleeper, ESPN, Yahoo, NFL) documents a "12-hour trade embargo"
or a named "self-dealing rule." The enforceable analogs are one-person-one-
team clauses and one-account-per-verified-person terms. Do not state either
invented rule as a platform fact.
