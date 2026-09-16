# Timeline: realistic examples

## Company milestones, ungrouped

What this shows: a flat timeline where each period gets its own color automatically (no sections defined).

```mermaid
timeline
    title Company Milestones
    2019 : Founded
    2020 : Seed round : First customer
    2021 : Series A
    2023 : 100th employee
    2025 : IPO
```

## Product roadmap grouped by quarter

What this shows: `section` grouping so each release quarter shares one color, with multiple bullet events per quarter on continuation lines.

```mermaid
timeline
    title 2026 Product Roadmap
    section Q1 — Foundation
        API v2 : Auth rework
               : Rate limiting
        Migration tooling : Zero-downtime cutover
    section Q2 — Expansion
        Multi-region : EU deployment
        Mobile SDK : iOS launch
                    : Android launch
```

## Onboarding process, top-down

What this shows: `TD` direction (v11.14.0+) for a vertical flow, better suited to embedding narrow in a doc column than the default left-right layout.

```mermaid
timeline TD
    title New Hire Onboarding
    Day 1 : Laptop setup : Team intro
    Week 1 : Codebase walkthrough : First PR
    Month 1 : First on-call shift
    Month 3 : 90-day review
```

## Historical eras with long event text and forced line breaks

What this shows: `<br>` for manual breaks inside long descriptive text, and multiple time periods sharing one section.

```mermaid
timeline
    title England's History Timeline
    section Stone Age
        7600 BC : Britain's oldest known house was built in Orkney, Scotland
        6000 BC : Sea levels rise and Britain becomes an island.<br>The people who live here are hunter-gatherers.
    section Bronze Age
        2300 BC : People arrive from Europe and settle in Britain.<br>They bring farming and metalworking.
                 : New styles of pottery and ways of burying the dead appear.
```
