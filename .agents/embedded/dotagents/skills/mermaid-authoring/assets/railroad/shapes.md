# Railroad diagram visual elements

From the official doc's "Visual Elements" section — how grammar constructs render, regardless of which notation (EBNF/ABNF/PEG/IR) produced them:

| Construct | Rendered as |
|---|---|
| Terminal (literal string) | rounded rectangle, theme-colored |
| Non-terminal (rule reference) | regular (square-cornered) rectangle, theme-colored |
| Sequence | left-to-right path connecting elements in order |
| Choice / alternation | branching curved paths, one per alternative |
| Optional | a branch that can bypass the element entirely |
| Repetition | a backward (loop) path returning to before the element |
| Rule start/end | small circles at the beginning and end of each rule's line |

All shapes inherit the active mermaid theme's colors and typography — there's no dedicated railroad color palette to pick shapes by color, styling is theme-driven only. Diagram-specific overrides go through the `railroad` config block, not per-shape `style` statements (railroad diagrams don't document `classDef`/`style` support).

Handdrawn look (`look: handDrawn`) is explicitly unsupported — shapes always render with clean, non-sketchy edges regardless of global look setting.
