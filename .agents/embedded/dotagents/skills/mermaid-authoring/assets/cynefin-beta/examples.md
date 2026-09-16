# Cynefin examples

## Strategy categorization with domain transitions

What this shows: sorting workstreams into domains and tracking how items move as understanding improves — the actual point of a Cynefin exercise, not just a static sort.

```mermaid
cynefin-beta
  title Strategy Categorization

  complex
    "Market research"

  complicated
    "Competitive analysis"

  clear
    "Standard pricing"

  chaotic
    "Crisis management"

  complex --> complicated : "Pattern identified"
  complicated --> clear : "Best practice codified"
  clear --> chaotic : "Complacency"
  chaotic --> complex : "Stabilized"
```

## Blank worksheet template

What this shows: an empty framework with no items, useful for live facilitation — project the diagram, then add items as a group discusses each one.

```mermaid
cynefin-beta
  title Cynefin Framework

  complex
  complicated
  clear
  chaotic
  confusion
```
