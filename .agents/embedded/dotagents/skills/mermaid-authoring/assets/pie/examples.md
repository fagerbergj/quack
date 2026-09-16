# Pie: realistic examples

## Browser market share

What this shows: a simple proportional breakdown with `showData` printing exact percentages next to the legend.

```mermaid
pie showData
    title Browser Market Share, Q2 2026
    "Chrome" : 65.2
    "Safari" : 18.3
    "Edge" : 5.1
    "Firefox" : 3.4
    "Other" : 8.0
```

## Support ticket categories

What this shows: categorical volume comparison, the classic pie use case.

```mermaid
pie title Support Tickets by Category (last 30 days)
    "Billing" : 142
    "Bug report" : 98
    "Feature request" : 61
    "Account access" : 44
    "Other" : 27
```

## Infrastructure cost breakdown as a donut chart

What this shows: `donutHole` config for a donut variant, useful when you want a center label area.

```mermaid
---
config:
  pie:
    donutHole: 0.4
    legendPosition: bottom
---
pie
    title Monthly Cloud Spend by Service
    "Compute" : 4200
    "Storage" : 1100
    "Networking" : 650
    "Database" : 2300
```

## Team survey results with a highlighted slice

What this shows: `highlightSlice` drawing attention to one category, e.g. the one requiring action.

```mermaid
---
config:
  pie:
    highlightSlice: Dissatisfied
---
pie showData
    title Team Retro Sentiment
    "Satisfied" : 24
    "Neutral" : 9
    "Dissatisfied" : 5
```
