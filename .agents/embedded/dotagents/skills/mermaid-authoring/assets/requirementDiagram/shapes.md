# Requirement Diagram — element vocabulary

All enumerations below are demonstrated in the official doc using lowercase values, despite the doc's reference table capitalizing them (see the README's Gotchas). Lowercase is the verified-working form.

## Requirement types

| Keyword | SysML meaning |
|---|---|
| `requirement` | generic requirement |
| `functionalRequirement` | behavior the system must perform |
| `interfaceRequirement` | boundary/interface constraint |
| `performanceRequirement` | timing, throughput, capacity constraint |
| `physicalRequirement` | physical/hardware constraint |
| `designConstraint` | constrains implementation choices |

## Risk levels

`risk: <value>` — one of: `Low`, `Medium`, `High` (doc examples use lowercase: `low`, `medium`, `high`).

## Verification methods

`verifymethod: <value>` — one of: `Analysis`, `Inspection`, `Test`, `Demonstration` (doc examples use lowercase: `analysis`, `inspection`, `test`, `demonstration`).

## Relationship types

Used in `{source} - <type> -> {destination}`:

| Type | Meaning |
|---|---|
| `contains` | source is composed of destination |
| `copies` | source is a copy of destination |
| `derives` | source is derived from destination |
| `satisfies` | source satisfies destination (e.g. an element satisfying a requirement) |
| `verifies` | source verifies destination |
| `refines` | source refines/adds detail to destination |
| `traces` | source traces to destination (generic link) |

## Element fields

`element` blocks take two freeform (not enumerated) fields:

```text
element user_defined_name {
    type: user_defined_type
    docref: user_defined_ref
}
```

`docref` is meant to point at a portion of another document — a file path, URL, or ticket id.
