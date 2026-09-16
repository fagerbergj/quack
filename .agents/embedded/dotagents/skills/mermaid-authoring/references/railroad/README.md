# Railroad Diagrams

- **Keyword(s):** `railroad-ebnf-beta` (EBNF), `railroad-abnf-beta` (ABNF), `railroad-peg-beta` (PEG), `railroad-beta` (raw IR primitives)
- **Introduced:** mermaid v11.16.0. **Beta** — all four keywords.
- **Use when:** documenting a context-free grammar (a language, a config format, a protocol) as a syntax/railroad diagram, in whichever notation your grammar is already written in.
- **Avoid when:** you're diagramming control flow or a process, not a grammar — use `flowchart`; also avoid if you need something a general audience can read without grammar-notation literacy.

## Minimal example

```mermaid
railroad-ebnf-beta
digit = "0" | "1" | "2" ;
```

## Core syntax

Pick the keyword matching the notation your grammar is already written in — don't transliterate, each notation has its own operators.

All four share: diagram-type keyword on the first line, optional `title "text"`, optional `accTitle:`/`accDescr:`, one rule per statement, statements end with `;`.

### EBNF (`railroad-ebnf-beta`)

Rules: `name = definition ;` (`::=` also accepted). Supports both W3C and ISO 14977 styles in the same notation:

| Feature | W3C | ISO 14977 |
|---|---|---|
| Terminal | `"text"` | `"text"` |
| Sequence | `A B` | `A , B` |
| Choice | `A \| B` | `A \| B` |
| Optional | `A?` | `[ A ]` |
| 0+ repetition | `A*` | `{ A }` |
| 1+ repetition | `A+` | — |
| Grouping | `( A B )` | `( A B )` |
| Comment | `/* text */` | `(* text *)` |
| Exception | `A - B` | `A - B` |

```mermaid
railroad-ebnf-beta
number = sign? digit+ ;
sign = "+" | "-" ;
digit = "0" | "1" | "2" ;
```

### ABNF (`railroad-abnf-beta`, RFC 5234)

Rules: `name = definition ;`. Alternation is `/` not `|`. Repetition is a prefix: `*A` (0+), `1*A` (1+), `2*4A` (2 to 4), `3A` (exactly 3). Terminals can be numeric: `%x41`, `%d65`, `%b1000001`, ranges `%x30-39`. Comments start with `;` to end of line.

```mermaid
railroad-abnf-beta
address = local-part "@" domain ;
local-part = 1*( ALPHA / DIGIT / "." / "-" ) ;
domain = label *( "." label ) ;
label = 1*( ALPHA / DIGIT / "-" ) ;
```

### PEG (`railroad-peg-beta`)

Rules: `Name <- definition ;`. Ordered choice `/` (first match wins). Suffixes `A?` `A*` `A+`. Prefix predicates `&A` (lookahead), `!A` (negative lookahead). `.` matches any char. Comments start with `#`.

```mermaid
railroad-peg-beta
Identifier <- !Keyword Letter Letter* ;
Keyword <- "if" / "else" / "while" ;
Letter <- "a" / "b" / "c" / "_" ;
```

### IR primitives (`railroad-beta`)

Rules: `name = expression ;` built from explicit constructors instead of grammar syntax — use when a layout doesn't map cleanly onto any single notation above.

| Constructor | Meaning |
|---|---|
| `terminal("text")` | literal |
| `nonterminal("name")` | rule reference |
| `sequence(a, b, ...)` | in order |
| `choice(a, b, ...)` | alternatives |
| `optional(a)` | zero or one |
| `zeroOrMore(a)` | 0+ |
| `oneOrMore(a)` | 1+ |
| `special("text")` | special sequence |

```mermaid
railroad-beta
digit = choice(terminal("0"), terminal("1"), terminal("2")) ;
```

## Gotchas

- Don't mix notations in one diagram — pick the keyword matching your grammar and stick to its operator set throughout.
- Handdrawn look (`look: handDrawn`) is not supported for any of the four keywords.
- ABNF and PEG both use `/` for alternation (different from EBNF's `|`) — easy to typo if you're switching between grammar styles.
- The IR primitive form (`railroad-beta`) has no shorthand operators at all — everything is an explicit constructor call, which is verbose but gives full control.

## Deeper

- `../../assets/railroad/shapes.md` — how each grammar construct (terminal, choice, repetition, grouping) maps to the rendered railroad shape.
- `../../assets/railroad/examples.md` — worked grammars (arithmetic expression, JSON, email address).
