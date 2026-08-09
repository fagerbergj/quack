# Class diagram vocabulary

## Relationship types

```text
[classA][Arrow][ClassB]
```

| Arrow | Meaning |
| --- | --- |
| `<\|--` | Inheritance |
| `*--` | Composition |
| `o--` | Aggregation |
| `-->` | Association |
| `--` | Link (solid) |
| `..>` | Dependency |
| `..\|>` | Realization |
| `..` | Link (dashed) |

Add a label after a colon: `classA <|-- classB : implements`. Arrowheads can point either direction: `classG <-- classH : Association`.

## Two-way relations

```text
[Relation Type][Link][Relation Type]
```

Relation type: `<|` inheritance, `*` composition, `o` aggregation, `>`/`<` association, `|>` realization. Link: `--` solid, `..` dashed. Example: `Animal <|--|> Zebra`.

## Lollipop interfaces

`bar ()-- foo` or `foo --() bar` draws a lollipop interface on the class. Each defined interface is unique - don't reuse one across classes.

## Cardinality / multiplicity

Placed in quotes near either end of an association: `Customer "1" --> "*" Ticket`.

| Notation | Meaning |
| --- | --- |
| `1` | Only 1 |
| `0..1` | Zero or one |
| `1..*` | One or more |
| `*` | Many |
| `n` | Exactly n (n>1) |
| `0..n` | Zero to n (n>1) |
| `1..n` | One to n (n>1) |

## Visibility markers (member prefix)

| Marker | Meaning |
| --- | --- |
| `+` | Public |
| `-` | Private |
| `#` | Protected |
| `~` | Package/internal |

## Classifiers (member suffix)

| Marker | Meaning | Example |
| --- | --- | --- |
| `*` | Abstract method | `someAbstractMethod()*` or `someAbstractMethod() int*` |
| `$` | Static method | `someStaticMethod()$` or `someStaticMethod() String$` |
| `$` | Static field | `String someField$` |

## Common annotations

`<<Interface>>`, `<<Abstract>>`, `<<Service>>`, `<<Enumeration>>` - freeform text between `<<` and `>>`, not a fixed enum.
