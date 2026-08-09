# XY Chart: realistic examples

## Monthly revenue: bar + line overlay

What this shows: actuals as bars with a trend line over them, the most common combo use.

```mermaid
xychart
    title "Sales Revenue"
    x-axis [jan, feb, mar, apr, may, jun, jul, aug, sep, oct, nov, dec]
    y-axis "Revenue (in $)" 4000 --> 11000
    bar [5000, 6000, 7500, 8200, 9500, 10500, 11000, 10200, 9200, 8500, 7000, 6000]
    line [5000, 6000, 7500, 8200, 9500, 10500, 11000, 10200, 9200, 8500, 7000, 6000]
```

## API latency percentiles over rolling windows

What this shows: multiple named line series for legend-driven comparison — a real observability dashboard pattern.

```mermaid
xychart-beta
    title "API Latency (ms)"
    x-axis ["90d", "60d", "30d", "7d", "1d", "Current"]
    y-axis "Milliseconds" 0 --> 200
    line "p50" [38.2, 36.8, 39.7, 54.5, 49.0, 38.4]
    line "avg" [48.1, 41.5, 45.7, 72.8, 67.7, 59.9]
    line "p95" [112.2, 75.3, 103.0, 177.0, 180.2, 109.4]
```

## Genre survey with data labels shown outside the bars

What this shows: `showDataLabelOutsideBar` for a chart where readers need the exact count, not just the visual comparison.

```mermaid
---
config:
    xyChart:
        showDataLabel: true
        showDataLabelOutsideBar: true
---
xychart
    title "Genres in Top 100 Book Survey of 2025"
    x-axis [comedy, romance, mystery, crime, "non fiction", other]
    y-axis "Number of Books" 0 --> 30
    bar [12, 2, 20, 25, 17, 24]
```

## Model size vs. capability milestone, with per-point labels

What this shows: per-point line labels (v11.16.0+) annotating specific milestones directly on the trend, avoiding a separate legend lookup.

```mermaid
xychart
    title "Smallest AI Models Scoring Above 60% on MMLU"
    x-axis "Date" ["Apr 2022", "Feb 2023", "Jul 2023", "Sep 2023", "Apr 2024"]
    y-axis "Parameters (B)" 0 --> 600
    line [540 "PaLM", 65 "LLaMA-65B", 34 "Llama 2 34B", 7 "Mistral 7B", 3.8 "Phi-3-mini"]
```
