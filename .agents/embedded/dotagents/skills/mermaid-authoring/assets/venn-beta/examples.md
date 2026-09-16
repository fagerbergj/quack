# Venn examples

## Three-way strategic overlap

What this shows: the classic "Innovation" framing (Desirable/Feasible/Viable) — a higher-arity union naming the sweet spot where all three sets overlap.

```mermaid
venn-beta
  set Desirable
  set Feasible
  set Viable
  union Desirable,Feasible,Viable["Innovation"]
```

## Sized sets with nested labels and styling

What this shows: combining `:N` sizing, per-set `text` labels, and `style` overrides in one diagram — the full feature set working together.

```mermaid
venn-beta
  set A["Frontend"]:20
    text A1["React"]
    text A2["Design Systems"]
  set B["Backend"]:12
    text B1["API"]
  union A,B["Shared"]:3
    text AB1["OpenAPI"]
  style A fill:#ff6b6b
  style A,B color:#333
```

## Two labeled sets, short identifiers

What this shows: keeping identifiers terse (`A`, `B`) while giving each a readable display label — useful when the same sets get referenced repeatedly in a longer diagram.

```mermaid
venn-beta
  set A["Alpha"]
  set B["Beta"]
  union A,B["AB"]
```
