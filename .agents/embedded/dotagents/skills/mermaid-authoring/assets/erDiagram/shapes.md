# ER diagram vocabulary

## Cardinality markers

Each marker has two characters: outer = maximum, inner = minimum. Placed at each end of the relationship line.

| Left | Right | Meaning |
| --- | --- | --- |
| `\|o` | `o\|` | Zero or one |
| `\|\|` | `\|\|` | Exactly one |
| `}o` | `o{` | Zero or more (no upper limit) |
| `}\|` | `\|{` | One or more (no upper limit) |

### Word aliases

| Alias | Meaning |
| --- | --- |
| `one or zero`, `zero or one` | Zero or one |
| `one or more`, `one or many`, `many(1)`, `1+` | One or more |
| `zero or more`, `zero or many`, `many(0)`, `0+` | Zero or more |
| `only one`, `1` | Exactly one |

Example using aliases: `CAR 1 to zero or more NAMED-DRIVER : allows`.

## Identifying vs. non-identifying relationships

`--` (solid line) = identifying: the child entity cannot exist without the parent. `..` (dashed line) = non-identifying: both entities can exist independently.

| Alias | Meaning |
| --- | --- |
| `to` | identifying |
| `optionally to` | non-identifying |

Example: `PERSON }|..|{ CAR : "driver"` (non-identifying, dashed) vs. a resolved join entity like `NAMED-DRIVER` that requires both `PERSON` and `CAR` (identifying, solid).

## Attribute keys

Placed after the attribute name inside an entity's `{}` block:

| Key | Meaning |
| --- | --- |
| `PK` | Primary key |
| `FK` | Foreign key |
| `UK` | Unique key |

Combine with a comma: `string carRegistrationNumber PK, FK`. A trailing double-quoted string after the key is a comment, e.g. `string driversLicense PK "The license #"`.
