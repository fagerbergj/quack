# Radar: realistic examples

## Candidate skills comparison

What this shows: two curves compared across shared skill dimensions, positional value syntax.

```mermaid
radar-beta
  title Interview Scorecard
  axis comm["Communication"], tech["Technical"], sys["System Design"]
  axis lead["Leadership"], culture["Culture Fit"]
  curve alice["Alice"]{8, 9, 7, 6, 8}
  curve bob["Bob"]{6, 7, 9, 8, 7}
  max 10
  min 0
```

## Restaurant comparison with a polygon graticule

What this shows: the doc's polygon graticule option, better suited to a small number of axes than the default circular grid.

```mermaid
radar-beta
  title Restaurant Comparison
  axis food["Food Quality"], service["Service"], price["Price"]
  axis ambiance["Ambiance"]
  curve a["Restaurant A"]{4, 3, 2, 4}
  curve b["Restaurant B"]{3, 4, 3, 3}
  curve c["Restaurant C"]{2, 3, 4, 2}
  graticule polygon
  max 5
```

## Product spec comparison using key:value curves

What this shows: key:value curve syntax, which avoids ordering bugs when specs are entered out of axis order.

```mermaid
radar-beta
  title Laptop Comparison
  axis battery["Battery Life"], perf["Performance"], weight["Portability"], price["Value"]
  curve modelA["Model A"]{ perf: 9, battery: 6, weight: 7, price: 5 }
  curve modelB["Model B"]{ battery: 9, perf: 6, weight: 8, price: 8 }
  max 10
```

## Service health across on-call metrics

What this shows: a single-curve radar used as a quick health snapshot rather than a comparison.

```mermaid
radar-beta
  title Service Health: checkout-api
  axis latency["Latency"], errors["Error Rate"], uptime["Uptime"], throughput["Throughput"]
  curve current["Current"]{7, 8, 9, 6}
  max 10
  min 0
  ticks 5
```
