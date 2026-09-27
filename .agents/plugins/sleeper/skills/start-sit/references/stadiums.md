# NFL venues, 2026 season

Where each game is played and whether weather can reach the field. A game
is at the **home** team's stadium from `sleeper_schedule` unless it is in
the neutral-site list below; a game not listed there that looks relocated
(weather, a playoff neutral site, a late NFL announcement) needs a quick
`web_search` to confirm the venue before trusting this table.

Roof: **open** = weather applies. **Dome** (fixed roof, including the
translucent "skylight" roofs at Allegiant, SoFi, and U.S. Bank) and
**retractable** (closed for bad weather; teams decide on game day) = skip
the forecast. Coordinates are for the `weather` tool's `latitude`/
`longitude`; TZ is the venue's local time zone, which `weather`'s `time`
expects (Arizona does not observe DST).

Source: [List of current NFL stadiums, Wikipedia](https://en.wikipedia.org/wiki/List_of_current_NFL_stadiums), checked Sep 2026.

| Team | Stadium | City | Roof | Lat, Lon | TZ |
| --- | --- | --- | --- | --- | --- |
| ARI | State Farm Stadium | Glendale, AZ | retractable | 33.53, -112.26 | MST |
| ATL | Mercedes-Benz Stadium | Atlanta, GA | retractable | 33.76, -84.40 | ET |
| BAL | M&T Bank Stadium | Baltimore, MD | open | 39.28, -76.62 | ET |
| BUF | Highmark Stadium (new, opened 2026) | Orchard Park, NY | open | 42.77, -78.79 | ET |
| CAR | Bank of America Stadium | Charlotte, NC | open | 35.23, -80.85 | ET |
| CHI | Soldier Field | Chicago, IL | open | 41.86, -87.62 | CT |
| CIN | Paycor Stadium | Cincinnati, OH | open | 39.10, -84.52 | ET |
| CLE | Huntington Bank Field | Cleveland, OH | open | 41.51, -81.70 | ET |
| DAL | AT&T Stadium | Arlington, TX | retractable | 32.75, -97.09 | CT |
| DEN | Empower Field at Mile High | Denver, CO | open | 39.74, -105.02 | MT |
| DET | Ford Field | Detroit, MI | dome | 42.34, -83.05 | ET |
| GB | Lambeau Field | Green Bay, WI | open | 44.50, -88.06 | CT |
| HOU | NRG Stadium | Houston, TX | retractable | 29.68, -95.41 | CT |
| IND | Lucas Oil Stadium | Indianapolis, IN | retractable | 39.76, -86.16 | ET |
| JAX | EverBank Stadium | Jacksonville, FL | open | 30.32, -81.64 | ET |
| KC | Arrowhead Stadium | Kansas City, MO | open | 39.05, -94.48 | CT |
| LAC | SoFi Stadium | Inglewood, CA | dome | 33.95, -118.34 | PT |
| LAR | SoFi Stadium | Inglewood, CA | dome | 33.95, -118.34 | PT |
| LV | Allegiant Stadium | Paradise, NV | dome | 36.09, -115.18 | PT |
| MIA | Hard Rock Stadium | Miami Gardens, FL | open | 25.96, -80.24 | ET |
| MIN | U.S. Bank Stadium | Minneapolis, MN | dome | 44.97, -93.26 | CT |
| NE | Gillette Stadium | Foxborough, MA | open | 42.09, -71.26 | ET |
| NO | Caesars Superdome | New Orleans, LA | dome | 29.95, -90.08 | CT |
| NYG | MetLife Stadium | East Rutherford, NJ | open | 40.81, -74.07 | ET |
| NYJ | MetLife Stadium | East Rutherford, NJ | open | 40.81, -74.07 | ET |
| PHI | Lincoln Financial Field | Philadelphia, PA | open | 39.90, -75.17 | ET |
| PIT | Acrisure Stadium | Pittsburgh, PA | open | 40.45, -80.02 | ET |
| SEA | Lumen Field | Seattle, WA | open | 47.60, -122.33 | PT |
| SF | Levi's Stadium | Santa Clara, CA | open | 37.40, -121.97 | PT |
| TB | Raymond James Stadium | Tampa, FL | open | 27.98, -82.50 | ET |
| TEN | Nissan Stadium | Nashville, TN | open | 36.17, -86.77 | CT |
| WAS | Northwest Stadium | Landover, MD | open | 38.91, -76.86 | ET |

## Neutral-site games, 2026

From the [NFL's 2026 international schedule](https://media.nfl.com/news-and-releases/international/nfl-unveils-2026-international-games-schedule).
Local kickoff is abroad, so convert from the US time `sleeper_schedule` gives.

| Date | Game (designated home) | Stadium | City | Roof | Lat, Lon | TZ |
| --- | --- | --- | --- | --- | --- | --- |
| Sep 10 | SF vs LAR (SF) | Melbourne Cricket Ground | Melbourne | open | -37.82, 144.98 | Australia/Melbourne |
| Sep 27 | BAL vs DAL (BAL) | Maracanã | Rio de Janeiro | open | -22.91, -43.23 | America/Sao_Paulo |
| Oct 4 | IND vs WAS (IND) | Tottenham Hotspur Stadium | London | open | 51.60, -0.07 | Europe/London |
| Oct 11 | PHI vs JAX (PHI) | Tottenham Hotspur Stadium | London | open | 51.60, -0.07 | Europe/London |
| Oct 18 | HOU vs JAX (HOU) | Wembley Stadium | London | open | 51.56, -0.28 | Europe/London |
| Oct 25 | PIT vs NO (PIT) | Stade de France | Saint-Denis (Paris) | open | 48.92, 2.36 | Europe/Paris |
| Nov 8 | CIN vs ATL (CIN) | Santiago Bernabéu | Madrid | retractable | 40.45, -3.69 | Europe/Madrid |
| Nov 15 | NE vs DET (NE) | Allianz Arena | Munich | open | 48.22, 11.62 | Europe/Berlin |
| Nov 22 | MIN vs SF (MIN) | Estadio Banorte | Mexico City | open | 19.30, -99.15 | America/Mexico_City |
