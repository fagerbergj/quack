# Cynefin Framework Diagram

- **Keyword(s):** `cynefin-beta`
- **Introduced:** mermaid v11.16.0. **Beta** — syntax may still change.
- **Use when:** categorizing problems/items into the five Cynefin sense-making domains (Clear, Complicated, Complex, Chaotic, Confusion) and optionally showing how items move between domains over time.
- **Avoid when:** you just need a generic 2x2 or quadrant chart — use `quadrantChart` instead; Cynefin's fixed five-domain layout only fits the actual Cynefin methodology.

## Minimal example

```mermaid
cynefin-beta
  complex
  complicated
  clear
  chaotic
```

## Core syntax

First line after any front matter must be `cynefin-beta`. Then:

| Construct | Meaning |
|---|---|
| `title Text` | optional diagram title |
| `complex` / `complicated` / `clear` / `chaotic` / `confusion` | opens a domain block; these are the only recognized domain names |
| `"Item label"` | quoted string on its own line inside a domain block, renders as a text badge |
| `domainA --> domainB : "label"` | top-level transition arrow between two domains, label optional |

```mermaid
cynefin-beta
  title Incident Response

  complex
    "Investigate root cause"
    "Run chaos experiment"

  complicated
    "Analyze performance data"

  clear
    "Restart service"

  chaotic
    "Page on-call immediately"

  confusion
    "Unknown failure mode"

  complex --> complicated : "Pattern identified"
  clear --> chaotic : "Complacency"
```

Domains can be declared in any order in the source — their on-canvas position is always fixed (Complex top-left, Complicated top-right, Chaotic bottom-left, Clear bottom-right, Confusion center). Domain blocks with no items still render (useful as a blank worksheet).

## Gotchas

- The `confusion` domain caps at 3 visible items; a 4th+ collapses into a `+N more` badge. The four main quadrants don't clip — keep item lists short anyway, the layout is fixed-size.
- Self-loop transitions (`complex --> complex`) are silently dropped — a transition must connect two different domains.
- Handdrawn look (`look: handDrawn`) is not supported.
- Domain names are fixed keywords — only the five listed are recognized, no custom domains.

## Deeper

See `../../assets/cynefin-beta/examples.md` for a full worked strategy-categorization example.
