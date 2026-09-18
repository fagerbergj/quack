# Weather and Vegas totals

Source: `research-r1-startsit.md` §1.5-1.7.

## Weather multipliers

Player-level, 2017 season NOAA data ([PFF, May 7, 2018](https://www.pff.com/news/fantasy-football-quantifying-weathers-impact-on-fantasy-performance)):

| Condition | QB Comp% | QB YPA | RB YPC |
| --- | --- | --- | --- |
| Light rain | −2.3% | −0.32 | +0.19 |
| Moderate rain | −3.4% | −0.33 | +0.04 |
| Snow | −4.1% | −0.07 | +0.089 |
| 30-49°F | −1.8% | −0.17 | +0.07 |
| <30°F | −3.1% | −0.19 | +0.26 |
| Wind 5-10mph | −0.7% | −0.13 | — |
| Wind 10+mph | −1.8% | −0.30 | — |

Modern re-tests (2018-2022 and 2014-2025, [Fantasy Life, 2024/2026](https://www.fantasylife.com/articles/fantasy/the-effects-of-weather-on-fantasy-football-2026))
find no visible decline in pass rate/plays until wind exceeds ~20mph, and
QB YPA craters 17.9% only past ~25mph sustained. Only ~3-3.7% of games see
wind that extreme or heavy precipitation.

**Null result to weigh against the above:** an ML-model fit found no
meaningful correlation between weather and raw fantasy points by position
([The Fantasy Footballers, Mar 8, 2024](https://www.thefantasyfootballers.com/analysis/does-weather-impact-fantasy-football-performance/)).

**Working rule:** do not adjust for wind <20-25mph, temperature >30°F, or
light rain. At genuine extremes (≥25mph sustained crosswind, heavy
precipitation, <30°F), trim QB/WR *efficiency* — not volume — using the
multipliers above, and do not bench a clearly-better player on weather
alone. No primary source publishes an exact sit threshold; state that
explicitly rather than citing an invented cutoff.

## Vegas totals

- Implied team total = (game total ± spread) / 2 — standard, verifiable
  arithmetic ([worked example, flagged secondary source](https://fantasystartsit.com/vegas-lines-and-game-totals/)).
- Predictive strength: the pre-game total correlates .32 with the actual
  combined score, the spread .42 with the final margin ([TFF, Jul 15, 2021](https://www.thefantasyfootballers.com/articles/the-fantasy-football-mythbusters-flip-the-game-script/)).
- **No primary or academic source publishes a total-points cutoff** for
  switching RB/WR priority in the FLEX or elsewhere. Numbers like "48+ =
  shootout" or "under 41 = grind" come from DFS-oriented or undated content
  — usable as rough color, never as a hard threshold, and always labeled as
  such.
- Double-counting risk: a total already embeds weather, injuries, and
  offensive quality. Using total + matchup rank + weather adjustment
  simultaneously over-adjusts. The Athletic's own instruction is to use
  matchup/total signals for close calls only, not to stack them.
