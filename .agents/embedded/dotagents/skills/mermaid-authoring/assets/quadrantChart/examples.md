# Quadrant Chart: realistic examples

## Eisenhower matrix for task triage

What this shows: the classic urgent/important prioritization grid, with quadrant labels doing the explanatory work and no data points at all.

```mermaid
quadrantChart
    x-axis Urgent --> Not Urgent
    y-axis Not Important --> Important
    quadrant-1 Plan
    quadrant-2 Do
    quadrant-3 Delegate
    quadrant-4 Delete
```

## Feature prioritization: effort vs. impact

What this shows: real data points bucketing product backlog items so the team can see what to build first.

```mermaid
quadrantChart
    title Backlog Prioritization
    x-axis Low Effort --> High Effort
    y-axis Low Impact --> High Impact
    quadrant-1 Do next
    quadrant-2 Big bets
    quadrant-3 Fill-ins
    quadrant-4 Reconsider
    SSO login: [0.3, 0.85]
    Dark mode: [0.2, 0.35]
    Realtime sync: [0.8, 0.9]
    Custom themes: [0.75, 0.2]
```

## Vendor comparison: cost vs. reliability, styled by tier

What this shows: styled points via classes to distinguish vendor tiers at a glance.

```mermaid
quadrantChart
    title Vendor Shortlist
    x-axis Low Cost --> High Cost
    y-axis Low Reliability --> High Reliability
    quadrant-1 Premium picks
    quadrant-2 Overpriced
    quadrant-3 Avoid
    quadrant-4 Best value
    Vendor A:::tierA: [0.8, 0.9]
    Vendor B:::tierB: [0.3, 0.6]
    Vendor C:::tierB: [0.2, 0.3]
    Vendor D:::tierA: [0.7, 0.4]
    classDef tierA color: #109060, radius: 10
    classDef tierB color: #908342, radius: 8
```

## Marketing campaign reach vs. engagement

What this shows: the doc's own domain example, extended with more campaigns to show real clustering.

```mermaid
quadrantChart
    title Reach and engagement of campaigns
    x-axis Low Reach --> High Reach
    y-axis Low Engagement --> High Engagement
    quadrant-1 We should expand
    quadrant-2 Need to promote
    quadrant-3 Re-evaluate
    quadrant-4 May be improved
    Campaign A: [0.3, 0.6]
    Campaign B: [0.45, 0.23]
    Campaign C: [0.57, 0.69]
    Campaign D: [0.78, 0.34]
    Campaign E: [0.40, 0.34]
    Campaign F: [0.35, 0.78]
```
