# C4 — element vocabulary

All elements: `Type(alias, label, ?description, ...)`. `_Ext` variants mark the element as external/outside your system boundary — renders in a different color.

## System Context

| Element | Signature |
|---|---|
| `Person` | `Person(alias, label, ?descr, ?sprite, ?tags, $link)` |
| `Person_Ext` | same |
| `System` | `System(alias, label, ?descr, ...)` |
| `SystemDb` | database-shaped system |
| `SystemQueue` | queue-shaped system |
| `System_Ext`, `SystemDb_Ext`, `SystemQueue_Ext` | external variants |
| `Enterprise_Boundary` | `Enterprise_Boundary(alias, label, ?tags, $link) { ... }` |
| `System_Boundary` | `System_Boundary(alias, label, ?tags, $link) { ... }` |
| `Boundary` | `Boundary(alias, label, ?type, ?tags, $link) { ... }` — generic boundary |

## Container

| Element | Signature |
|---|---|
| `Container` | `Container(alias, label, ?techn, ?descr, ...)` |
| `ContainerDb`, `ContainerQueue` | shaped variants |
| `Container_Ext`, `ContainerDb_Ext`, `ContainerQueue_Ext` | external variants |
| `Container_Boundary` | `Container_Boundary(alias, label, ?tags, $link) { ... }` |

## Component

| Element | Signature |
|---|---|
| `Component` | `Component(alias, label, ?techn, ?descr, ...)` |
| `ComponentDb`, `ComponentQueue` | shaped variants |
| `Component_Ext`, `ComponentDb_Ext`, `ComponentQueue_Ext` | external variants |

## Deployment

| Element | Signature |
|---|---|
| `Deployment_Node` | `Deployment_Node(alias, label, ?type, ?descr, ...) { ... }` — nests arbitrarily deep |
| `Node` | short alias for `Deployment_Node` |
| `Node_L`, `Node_R` | left/right-aligned `Node` |

## Relationships

| Relationship | Meaning |
|---|---|
| `Rel(from, to, label, ?techn, ...)` | directed, default direction |
| `BiRel(from, to, label, ...)` | bidirectional |
| `Rel_U`/`Rel_Up`, `Rel_D`/`Rel_Down`, `Rel_L`/`Rel_Left`, `Rel_R`/`Rel_Right` | directed, hints layout direction |
| `Rel_Back` | reversed arrowhead |
| `RelIndex(index, from, to, label, ...)` | numbered relationship (for `C4Dynamic`) — the `index` argument is accepted for C4-PlantUML compatibility but ignored; sequence numbers follow statement order |

## Risk / verification

C4 has no risk or verification-method vocabulary — that belongs to `requirementDiagram`.

## Styling calls (not elements, but element-scoped)

- `UpdateElementStyle(alias, $fontColor=, $bgColor=, $borderColor=, ...)`
- `UpdateRelStyle(from, to, $textColor=, $lineColor=, $offsetX=, $offsetY=)`
- `UpdateLayoutConfig($c4ShapeInRow=, $c4BoundaryInRow=)` — shapes/boundaries per row, defaults 4 and 2
