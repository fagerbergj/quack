# XY Chart

- **Keyword(s):** `xychart`, `xychart-beta`
- **Introduced:** core — in mermaid since 10.9.6 or earlier (verified against the v10.9.6 diagram registry),
  so it renders on effectively any deployed mermaid — the doc gives no introduction version.
  The doc itself is inconsistent about beta status:
  most examples use plain `xychart`,
  but the "Legend" example uses `xychart-beta`,
  and that section's own version tag is an unrendered template placeholder (`v<MERMAID_RELEASE_VERSION>+`) rather than a real number —
  treat as **effectively unstable/beta** until you confirm your renderer's behavior.
  Bar and line data-label features are v11.14.0+; per-point line labels are v11.16.0+.
- **Use when:** plotting numeric or categorical series on a real x/y axis — bar, line, or both together.
- **Avoid when:** you have no axis semantics (just proportions) — use `pie`;
  or a 2x2 bucket grid without a numeric scale — use `quadrantChart`.

## Minimal example

```mermaid
xychart
    title "Sales Revenue"
    x-axis [jan, feb, mar, apr, may, jun]
    y-axis "Revenue (in $)" 4000 --> 11000
    bar [5000, 6000, 7500, 8200, 9500, 10500]
    line [5000, 6000, 7500, 8200, 9500, 10500]
```

## Core syntax

- `xychart` or `xychart horizontal` (default orientation is vertical).
- `title "<text>"` — quote if it contains a space; single words don't need quotes.
- `x-axis`: either a numeric range (`x-axis title min --> max`) or categories (`x-axis "title" [cat1, "cat two", cat3]`).
  Optional — axis range auto-generates from data if omitted.
- `y-axis`: numeric only, `y-axis title min --> max` or just `y-axis title` (range auto-generated). Optional.
- `bar [v1, v2, ...]` and `line [v1, v2, ...]` plot series;
  prefix with a quoted name (`bar "series name" [...]`) to add it to the legend —
  unnamed series are omitted from the legend.
- Multiple `bar`/`line` statements layer on the same chart.
- Text values with spaces need double quotes; single-word values don't.

## Per-point line labels (v11.16.0+)

Append a quoted label after any numeric value in a `line` series;
labels are optional per point (mix labeled and unlabeled freely).
Rendered above the point (vertical orientation) or to the right (horizontal).
Fixed 12px font.
Not supported on `bar` — the syntax is accepted but labels are silently ignored there.

```mermaid
xychart
    title "Smallest AI models scoring above 60% on MMLU"
    x-axis "Date" ["Apr 2022", "Feb 2023", "Jul 2023", "Sep 2023", "Apr 2024"]
    y-axis "Parameters (B)" 0 --> 600
    line [540 "PaLM", 65 "LLaMA-65B", 34 "Llama 2 34B", 7 "Mistral 7B", 3.8 "Phi-3-mini"]
```

## Data labels on bars (v11.14.0+, config only)

`showDataLabel: true` (inside the bar) and `showDataLabelOutsideBar: true` (moves it outside)
go in `config.xyChart`, not in the diagram body:

```mermaid
---
config:
    xyChart:
        showDataLabel: true
        showDataLabelOutsideBar: true
---
xychart
    title "Genres in top 100 book survey"
    x-axis [comedy, romance, mystery, crime]
    y-axis "Number of Books" 0 --> 30
    bar [12, 2, 20, 25]
```

## Gotchas

- The doc's own template placeholder for the Legend feature version (`v<MERMAID_RELEASE_VERSION>+`) never got substituted —
  treat any behavior around named-series legends as unverified on older pinned mermaid versions.
- `y-axis` is numeric-only; you cannot give it categories — only `x-axis` supports categorical values.
- If you omit both axes, ranges are inferred from the data —
  fine for a quick sketch, risky if you need a stable axis across regenerated diagrams.
- Point labels only work on `line`, not `bar`;
  putting a label on a bar value is silently ignored rather than erroring.

## Deeper

See `../../assets/xychart/examples.md` for realistic examples.
