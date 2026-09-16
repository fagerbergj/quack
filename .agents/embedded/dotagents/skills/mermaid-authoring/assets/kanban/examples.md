# Kanban — examples

## 1. Sprint board with ticket links

What this shows: a standard three-column sprint board with ticket ids linked out via `ticketBaseUrl`, assignees, and priority.

```mermaid
---
config:
  kanban:
    ticketBaseUrl: 'https://example.atlassian.net/browse/#TICKET#'
---
kanban
  Backlog
    [Write onboarding docs]
  todo[In Progress]
    id1[Fix login redirect bug]@{ ticket: ENG-101, assigned: 'amy', priority: 'High' }
    id2[Add rate limiting]@{ ticket: ENG-102, assigned: 'ben' }
  done[Done]
    id3[Upgrade Postgres]@{ ticket: ENG-88, priority: 'Very High' }
```

## 2. Hiring pipeline

What this shows: kanban used for a non-software workflow — candidates moving through interview stages.

```mermaid
kanban
  applied[Applied]
    c1[Alex Rivera - Backend Engineer]
    c2[Jordan Lee - Designer]
  screen[Phone Screen]
    c3[Sam Patel - Backend Engineer]@{ priority: 'High' }
  onsite[Onsite]
    c4[Taylor Kim - Designer]@{ assigned: 'hiring-manager' }
  offer[Offer]
    c5[Morgan Diaz - Backend Engineer]@{ priority: 'Very High' }
```

## 3. Content publishing workflow

What this shows: an editorial board with assignees and priority, no ticket linking.

```mermaid
kanban
  draft[Draft]
    p1[Q3 roadmap post]
  review[In Review]
    p2[Migration guide]@{ assigned: 'editor-priya' }
  scheduled[Scheduled]
    p3[Launch announcement]@{ priority: 'High' }
  published[Published]
    p4[API changelog]
    p5[Security advisory]@{ priority: 'Very High' }
```
