# Wardley Map visual elements

## Node types

| Element | Rendering |
|---|---|
| `component` | plain dot/point positioned at `[visibility, evolution]`, with a text label |
| `anchor` | dot styled with a bold label, representing a user or customer at the top of the value chain |
| `pipeline` child | dot positioned along the parent's evolution axis, sharing the parent's visibility |

## Decorator symbols (sourcing strategy)

Appended after a component's coordinate as `(keyword)`:

| Decorator | Symbol |
|---|---|
| `(build)` | triangle |
| `(buy)` | diamond |
| `(outsource)` | square |
| `(market)` | circle |
| `(inertia)` | marks the component as resistant to change (visual treatment separate from the sourcing shapes above) |

## Links vs. flows

- `A -> B` / `A --> B`: plain dependency line, no directional flow marker.
- `A +> B`: flow, arrow marker in the forward direction.
- `A +< B`: flow, arrow marker in the reverse direction.
- `A +<> B`: bidirectional flow, arrows both ends.
- `A +'label'> B`: forward flow with an inline text label on the link.
- `A -.-> B`: dashed variant of the basic link.

## Evolution and trend markers

- `evolve Name targetEvo`: draws a red dashed arrow from the component's current position to its target evolution — the visual signal that a component is expected to mature.
- `Component -.- (x, y)`: trend indicator showing a predicted future position; uses plain `(x, y)` order, not `[visibility, evolution]` — the one shape whose coordinate convention differs from every other construct on the map.

## Annotations and notes

- `note "text" [vis, evo]`: free-floating text at a coordinate, no numbering, no connecting line.
- `annotation N,[x,y] "text"`: numbered marker at the coordinate; pair with `annotations [x, y]` to also place a legend box listing all numbered annotations.

## Accelerators / deaccelerators

- `accelerator "text" [vis, evo]`: marks a force speeding up evolution at that point on the map.
- `deaccelerator "text" [vis, evo]`: marks a force slowing or resisting evolution.

Handdrawn/rough mode is not supported for any of the above — Wardley uses a dedicated D3 renderer outside the shared mermaid shape system.
