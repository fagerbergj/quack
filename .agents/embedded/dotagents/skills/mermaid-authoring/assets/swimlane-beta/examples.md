# Swimlanes — examples

## 1. Incident escalation

What this shows: a customer-reported outage escalating from Support to SRE only for high-severity cases, with the resolution routed back through Support.

```mermaid
swimlane-beta LR
  subgraph Customer
    report[Report outage]
    notified[Receive status update]
  end

  subgraph Support
    triage[Triage severity]
    update[Post status update]
  end

  subgraph SRE
    investigate[Investigate root cause]
    mitigate[Apply mitigation]
  end

  report --> triage
  triage -->|Sev1| investigate
  triage -->|Minor| update
  investigate --> mitigate --> update
  update --> notified
```

## 2. Procurement approval

What this shows: a spend-approval decision owned by the Manager lane, with a highlighted class marking the risk point and a reject path back to the requester.

```mermaid
swimlane-beta LR
  subgraph requester [Requester]
    submit[Submit purchase request]
    receive[Receive goods]
  end

  subgraph manager [Manager]
    approve{Approve spend?}
  end

  subgraph procurement [Procurement]
    order[Place order with vendor]
  end

  submit --> approve
  approve -->|Yes| order
  approve -->|No| submit
  order --> receive

  classDef risky fill:#fff2cc,stroke:#d6a500,color:#111;
  class approve risky;
```

## 3. Order fulfillment with accessibility metadata

What this shows: a top-to-bottom fulfillment process using `accTitle`/`accDescr` for screen readers.

```mermaid
swimlane-beta TB
  accTitle: Order fulfillment
  accDescr: An order moves from the customer through the warehouse and carrier back to the customer.

  subgraph Customer
    place[Place order]
    receive[Receive package]
  end

  subgraph Warehouse
    pick[Pick items]
    pack[Pack box]
  end

  subgraph Carrier
    collect[Collect package]
    deliver[Deliver package]
  end

  place --> pick --> pack --> collect --> deliver --> receive
```

## 4. Hiring pipeline with a loop-back

What this shows: a decision made in the Hiring Manager lane routing back to earlier lanes on rejection — decisions live in the lane that owns them, and their outcomes route to whichever lane acts next.

```mermaid
swimlane-beta LR
  subgraph candidate [Candidate]
    apply[Submit application]
    interview[Attend interview]
  end

  subgraph recruiter [Recruiter]
    screen[Screen resume]
    schedule[Schedule interview]
  end

  subgraph hm [Hiring Manager]
    decide{Extend offer?}
    offer[Send offer]
  end

  apply --> screen
  screen -->|Pass| schedule --> interview --> decide
  screen -->|Fail| apply
  decide -->|Yes| offer
  decide -->|No| apply
```
