# Venue coordinates

Each player's own `game` carries its `venue` and `roof`, including
international and other neutral-site games. This table turns that `venue`
into coordinates for the `weather` tool; don't geocode a city instead
("Santa Clara" resolves to Cuba).

Roof comes from `game.roof`: skip `dome` and `retractable_dome`,
check `outdoor` - except where the Note column overrides Sleeper's value.
A venue missing here: `web_search` its coordinates, then pass them as
`latitude`/`longitude`.

Venue names and coordinates are the ones Sleeper's 2026 schedule reports
(Tottenham's longitude sign corrected).

| Venue (as Sleeper reports it) | City | Lat, Lon | Note |
| --- | --- | --- | --- |
| Acrisure Stadium | Pittsburgh | 40.45, -80.02 | |
| Allegiant Stadium | Las Vegas | 36.09, -115.18 | |
| Allianz Arena | Munich | 48.22, 11.62 | |
| Arrowhead Stadium | Kansas City | 39.05, -94.48 | |
| AT&T Stadium | Arlington | 32.75, -97.09 | |
| Bank of America Stadium | Charlotte | 35.23, -80.85 | |
| Caesars Superdome | New Orleans | 29.95, -90.08 | |
| Empower Field at Mile High | Denver | 39.74, -105.02 | |
| Estadio Banorte | Mexico City | 19.30, -99.15 | |
| EverBank Stadium | Jacksonville | 30.32, -81.64 | |
| Ford Field | Detroit | 42.34, -83.05 | |
| Gillette Stadium | Foxborough | 42.09, -71.26 | |
| Hard Rock Stadium | Miami Gardens | 25.96, -80.24 | |
| Highmark Stadium | Orchard Park | 42.77, -78.79 | new stadium, opened 2026 |
| Huntington Bank Field | Cleveland | 41.51, -81.70 | |
| Lambeau Field | Green Bay | 44.50, -88.06 | |
| Levi's Stadium | Santa Clara | 37.40, -121.97 | |
| Lincoln Financial Field | Philadelphia | 39.90, -75.17 | |
| Lucas Oil Stadium | Indianapolis | 39.76, -86.16 | |
| Lumen Field | Seattle | 47.60, -122.33 | |
| M&T Bank Stadium | Baltimore | 39.28, -76.62 | |
| Maracanã Stadium | Rio de Janeiro | -22.91, -43.23 | |
| Melbourne Cricket Ground | Melbourne | -37.82, 144.99 | |
| Mercedes-Benz Stadium | Atlanta | 33.76, -84.40 | |
| MetLife Stadium | East Rutherford | 40.81, -74.07 | |
| Nissan Stadium | Nashville | 36.17, -86.77 | |
| Northwest Stadium | Landover | 38.91, -76.86 | |
| NRG Stadium | Houston | 29.68, -95.41 | Sleeper currently reports it as Reliant Stadium |
| Paycor Stadium | Cincinnati | 39.10, -84.52 | |
| Raymond James Stadium | Tampa | 27.98, -82.50 | |
| Reliant Stadium | Houston | 29.68, -95.41 | Sleeper's old name for NRG Stadium |
| Santiago Bernabéu Stadium | Madrid | 40.45, -3.69 | |
| SoFi Stadium | Inglewood | 33.95, -118.34 | Sleeper says `outdoor`; the fixed roof covers the field - treat as dome |
| Soldier Field | Chicago | 41.86, -87.62 | |
| Stade de France | Saint-Denis | 48.92, 2.36 | |
| State Farm Stadium | Glendale | 33.53, -112.26 | |
| Tottenham Hotspur Stadium | London | 51.60, -0.07 | |
| U.S. Bank Stadium | Minneapolis | 44.97, -93.26 | |
| Wembley Stadium | London | 51.56, -0.28 | Sleeper says `retractable_dome`; the roof covers only the stands - treat as outdoor |
