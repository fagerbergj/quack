# Wardley Map

- **Keyword(s):** `wardley-beta`
- **Introduced:** mermaid v11.14.0. **Beta.**
- **Use when:** doing Wardley Mapping — positioning components on visibility x evolution axes to reason about build/buy/outsource decisions, dependencies, and where a component sits on its evolution curve.
- **Avoid when:** you just want a dependency graph with no strategic-evolution dimension — use `flowchart`; Wardley's coordinate system and vocabulary (anchor, evolve, inertia) only pay off if you're actually doing the mapping technique.

## Minimal example

```mermaid
wardley-beta
title Tea Shop Value Chain

anchor Business [0.95, 0.63]
component Cup of Tea [0.79, 0.61]
Business -> Cup of Tea
```

## Core syntax

**Coordinates are `[visibility, evolution]`** — the OnlineWardleyMaps (OWM) convention, and the opposite order of a typical `(x, y)` pair. Visibility is the Y-axis (0 = infrastructure, 1 = user-facing); evolution is the X-axis (0 = genesis, 1 = commodity).

| Element | Syntax | Example |
|---|---|---|
| Title | `title Text` | `title My Map` |
| Canvas size | `size [w, h]` | `size [1100, 800]` (default `[1100, 600]`) |
| Component | `component Name [vis, evo]` | `component API [0.6, 0.7]` |
| Anchor (user/customer) | `anchor Name [vis, evo]` | `anchor Customer [0.9, 0.95]` |
| Link | `A -> B` or `A --> B` | `API -> Database` |
| Flow (with arrow marker) | `A +> B`, `A +< B`, `A +<> B`, `A +'label'> B` | `API +<> Cache` |
| Evolve (movement) | `evolve Name targetEvo` | `evolve API 0.85` |
| Note | `note "text" [vis, evo]` | `note "Key insight" [0.4, 0.5]` |
| Annotation | `annotation N,[x,y] "text"` | `annotation 1,[0.5,0.5] "Critical"` |
| Pipeline | `pipeline Parent { component "X" [evo] ... }` | groups evolution-stage variants of one component |
| Custom evolution stages | `evolution Stage1 -> Stage2 -> ...` | replaces default Genesis/Custom/Product/Commodity labels |
| Label offset | `component Name [vis, evo] label [dx, dy]` | nudges the text away from its dot |
| Decorators | `(inertia)`, `(build)`, `(buy)`, `(outsource)`, `(market)` | append after the coordinate |

```mermaid
wardley-beta
title Sourcing Strategy

anchor Customer [0.80, 0.95]
component Custom App [0.45, 0.85] (build)
component Off-the-shelf Tool [0.85, 0.65] (buy)
component Managed Service [0.60, 0.40] (outsource)
component Cloud Platform [0.95, 0.25] (market)

Customer -> Custom App
Custom App -> Off-the-shelf Tool
```

Names with hyphens (`real-time processing`) don't need quoting; quote a name only if it starts with a non-letter or has a character the grammar can't otherwise parse. Trend indicators use plain `(x, y)` order (not `[visibility, evolution]`) — the one place the coordinate convention flips.

## Gotchas

- **Coordinate order is inverted from intuition**: `[visibility, evolution]`, not `[x, y]`/`[evolution, visibility]`. Every component/anchor/note/annotation uses this order; only the trend indicator (`Component -.- (x, y)`) uses standard `(x, y)`. Mixing these up silently plots things in the wrong place, no error.
- Handdrawn/rough mode (`look: handDrawn`) is not supported — Wardley uses its own D3 renderer, not the shared shape system other mermaid types share.
- Pipeline children only vary by evolution (X) — they inherit the parent's visibility (Y), so you can't give pipeline stages different Y positions.
- The reverse-flow/bidirectional flow markers (`+<`, `+<>`) are easy to transpose with the forward marker (`+>`) — double check direction against the actual dependency.

## Deeper

- `../../assets/wardley-beta/shapes.md` — anchor/component dot styles, decorator symbols (build/buy/outsource/market), and evolve-arrow rendering.
- `../../assets/wardley-beta/examples.md` — a complete multi-feature map (pipeline, custom evolution stages, annotations, notes together).
