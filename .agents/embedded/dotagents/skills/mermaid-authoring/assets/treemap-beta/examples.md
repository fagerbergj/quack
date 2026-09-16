# Treemap: realistic examples

## Disk usage by top-level folder

What this shows: the canonical treemap use case — nested filesystem sizes.

```mermaid
treemap-beta
"var"
    "log": 4200
    "cache": 1800
"home"
    "jason": 32000
    "shared": 5400
"opt"
    "app": 2100
```

## Org budget by department and line item, formatted as currency

What this shows: `valueFormat` making raw numbers read as dollar figures, a common request when the audience is non-technical.

```mermaid
---
config:
  treemap:
    valueFormat: '$0,0'
---
treemap-beta
"Engineering"
    "Salaries": 2400000
    "Cloud infra": 680000
    "Tooling": 120000
"Sales"
    "Commissions": 950000
    "Travel": 140000
"Marketing"
    "Ads": 600000
    "Events": 180000
```

## Product portfolio revenue with a highlighted underperformer

What this shows: `:::class` styling to flag one leaf (e.g. a product missing target) without touching the rest of the tree.

```mermaid
treemap-beta
"Consumer"
    "Widget Pro": 4200000
    "Widget Mini":::underperforming: 380000
"Enterprise"
    "Widget Enterprise": 6100000
    "Widget API": 1900000

classDef underperforming fill:#f96,stroke:#333,stroke-width:2px;
```

## Market share by company as a percentage

What this shows: `.1%` value formatting for a proportions-only treemap where the raw numbers are already fractions.

```mermaid
---
config:
  treemap:
    valueFormat: '.1%'
---
treemap-beta
"Market"
    "Company A": 0.35
    "Company B": 0.25
    "Company C": 0.15
    "Others": 0.25
```
