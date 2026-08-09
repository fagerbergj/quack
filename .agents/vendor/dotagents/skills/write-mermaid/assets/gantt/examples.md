# Gantt: realistic examples

## Software release schedule with dependencies and critical path

What this shows: cross-team phases where later work depends on earlier work finishing, plus a critical-path task and a release milestone.

```mermaid
gantt
    title Q3 Release Plan
    dateFormat YYYY-MM-DD
    excludes weekends

    section Design
    Spec review          :done, spec, 2026-07-01, 5d
    API design            :crit, active, api, after spec, 5d

    section Implementation
    Backend               :crit, backend, after api, 10d
    Frontend               :frontend, after api, 12d

    section QA
    Integration testing    :crit, qa, after backend frontend, 5d
    Release                :milestone, after qa, 0d
```

## Conference day agenda (time-of-day, no calendar dates)

What this shows: `HH:mm` `dateFormat` for a single-day schedule with milestones marking session boundaries.

```mermaid
gantt
    dateFormat HH:mm
    axisFormat %H:%M
    title Conference Day 1

    section Morning
    Doors open          :milestone, 08:30, 0m
    Keynote              :09:00, 45m
    Break                :09:45, 15m
    Workshop A            :10:00, 90m

    section Afternoon
    Lunch                :12:00, 60m
    Workshop B            :13:00, 90m
    Closing               :milestone, 15:00, 0m
```

## Hiring pipeline with excluded weekends

What this shows: real calendar duration accounting for weekends via `excludes`, with a task blocked until another completes via `until`.

```mermaid
gantt
    title Hiring Pipeline — Senior Engineer Req
    dateFormat YYYY-MM-DD
    excludes weekends

    section Sourcing
    Source candidates     :active, src, 2026-07-06, 10d

    section Interviews
    Phone screens          :screens, after src, 5d
    Onsite loop             :onsite, after screens, 8d
    Offer prep               :prep, 2026-07-20, until onsite

    section Close
    Offer extended          :milestone, after onsite, 0d
```

## Compact multi-track sprint board

What this shows: `displayMode: compact` packing several short tasks per section row, useful when many small tasks would otherwise sprawl vertically.

```mermaid
---
displayMode: compact
---
gantt
    title Sprint 42
    dateFormat YYYY-MM-DD

    section Backend
    Auth refactor    :a1, 2026-07-06, 3d
    Rate limiting     :a2, 2026-07-10, 2d
    DB migration       :a3, 2026-07-13, 2d

    section Frontend
    Settings page     :b1, 2026-07-06, 4d
    Dark mode          :b2, 2026-07-11, 3d
```
