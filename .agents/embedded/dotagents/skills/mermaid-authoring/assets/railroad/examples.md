# Railroad examples

## EBNF: arithmetic expression grammar

What this shows: standard left-recursion-free expression grammar with precedence (expression -> term -> factor), the shape most language grammars start from.

```mermaid
railroad-ebnf-beta
title "Arithmetic Expression Grammar"

expression = term ( ( "+" | "-" ) term )* ;
term = factor ( ( "*" | "/" ) factor )* ;
factor = number | "(" expression ")" ;
number = digit+ ;
digit = "0" | "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9" ;
```

## EBNF: JSON grammar

What this shows: documenting a real-world interchange format's syntax — the same style of grammar JSON.org itself publishes as a railroad diagram.

```mermaid
railroad-ebnf-beta
title "JSON Grammar"

json = element ;
element = object | array | string | number | "true" | "false" | "null" ;
object = "{" [ member ( "," member )* ] "}" ;
array = "[" [ element ( "," element )* ] "]" ;
member = string ":" element ;
```

## ABNF: email address (RFC-style)

What this shows: ABNF's characteristic prefix-repetition (`1*(...)`) for "one or more of a character class," the notation IETF specs use.

```mermaid
railroad-abnf-beta
title "Email Address"

address = local-part "@" domain ;
local-part = 1*( ALPHA / DIGIT / "." / "-" ) ;
domain = label *( "." label ) ;
label = 1*( ALPHA / DIGIT / "-" ) ;
```

## PEG: keyword-excluding identifier

What this shows: PEG's negative-lookahead predicate (`!Keyword`), which has no direct EBNF/ABNF equivalent — the reason to reach for PEG specifically when a grammar needs "match X but not if Y would also match here."

```mermaid
railroad-peg-beta
title "Identifiers (keywords excluded)"

Identifier <- !Keyword Letter Letter* ;
Keyword <- "if" / "else" / "while" ;
Letter <- "a" / "b" / "c" / "_" ;
```
