# Consolidation: the 2-for-1 rules

Source: `report.md` (R3 v2) section 1.2, plus one supplemental figure from
`research-r3-trades.md` (R3 v1) marked below.

## The package does not sum at 100%

[Fantasy Trade Pilot 2-for-1 calculator (values updated Aug 7, 2026)](https://fantasytradepilot.com/2-for-1-fantasy-football-trade-calculator):
"The highest-value asset receives full weight. The second asset receives
**82% by default**. Deeper formats may justify a smaller discount; shallow
starting lineups may require a larger one" — this league's 10-team start-9
is the shallow case the source flags for a bigger discount than 82%.

Supplemental (R3 v1): [PFF (2021-09-15, pre-2024 — the only published flat
2-for-1 premium figure)](https://www.pff.com/news/fantasy-football-week-2-trade-value-chart-2021):
"the side giving up the most players should expect to pay a premium of
5-10% over the stud player's cost" — a different, smaller number than the
82%-second-asset-weight rule above; name which figure is being used rather
than blending them.

[SportsMonkie (undated)](https://sportsmonkie.com/tools/fantasy-football-trade-calculator/)
publishes a fully worked decay formula, `value(i) × λ^(i−1)`, with
**λ = 0.66 at 8-team, 0.75 at 12-team, 0.85 at 16-team** — the shallower the
league, the stronger the discount. In a 12-team league this puts the 2nd
asset at 75%, 3rd at 56%, 4th at 42%.

## Price the forced cut

A 2-for-1 forces a roster cut. Include the dropped player's value as a
hidden third line item in the trade math ([Fantasy Trade Pilot](https://fantasytradepilot.com/2-for-1-fantasy-football-trade-calculator)).
Measure the *second upgrade*, not the second player in isolation — compare
the incoming player against whoever currently occupies that lineup spot, not
against the bench in the abstract (same source).

## Why the premium concentrates at RB

[FFTradeAnalyzer (undated)](https://www.fftradeanalyzer.com/guides/position-scarcity):
"The RB12 might score 15 PPG while RB24 scores 8 PPG — a 7-point gap. The
WR12 scores 14 PPG while WR24 scores 11 PPG — only 3 points," and "a WR2 for
RB2 swap typically favors whoever gets the RB." Only 8-12 RBs consistently
get 15+ touches per game (same source).

## When taking two is actually right

[Fantasy Trade Pilot](https://fantasytradepilot.com/2-for-1-fantasy-football-trade-calculator):
you have two genuine roster weaknesses; both incoming roles are startable
now; replacement options on waivers are poor; you're rebuilding and want a
liquid asset; or the package includes enough premium to clear the
consolidation discount.

## Bad-advice pattern

Naive additive sum on a 2-for-1 — summing chart values on both sides with no
second-asset discount and no forced cut priced — is the report's bad-advice
pattern #3. [4for4's own builder concedes the flaw](https://www.4for4.com/2022/preseason/fantasy-football-trade-value-models-what-why-how-and-who-cares)
(2022-08-23, pre-2024): lopsided 3-for-1 trades break naive additive math,
which is why the discount exists at all.
