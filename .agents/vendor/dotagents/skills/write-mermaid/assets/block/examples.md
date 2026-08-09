# Block — examples

## 1. Frontend/backend/database architecture

What this shows: a small web system laid out on a fixed 3-column grid, with directional block arrows carrying the flow and classes coloring the tiers.

```mermaid
block
  columns 3
  Frontend blockArrowId6<[" "]>(right) Backend
  space:2 down<[" "]>(down)
  Disk left<[" "]>(left) Database[("Database")]

  classDef front fill:#696,stroke:#333;
  classDef back fill:#969,stroke:#333;
  class Frontend front
  class Backend,Database back
```

## 2. API gateway with a composite backend block

What this shows: a gateway routing to an auth service and a nested `Backend` composite block holding two internal services, all landing in a shared database.

```mermaid
block
  columns 3
  Client space:2
  down1<[" "]>(down) space:2
  Gateway["API Gateway"] right1<[" "]>(right) Auth["Auth Service"]
  down2<[" "]>(down) space:2
  block:Backend
    columns 1
    Orders["Orders Service"]
    Billing["Billing Service"]
  end
  space:2
  Backend --> DB[("Primary DB")]

  classDef edge fill:#69c,stroke:#333;
  class Client,Gateway edge
```

## 3. Support ticket approval flow

What this shows: a decision-driven process flow using rhombus, circle, and block-arrow shapes with labeled edges for yes/no branches.

```mermaid
block
  columns 3
  Received(("Ticket received")) space:2
  d1<[" "]>(down) space:2
  Triage{{"Needs approval?"}} yes<["Yes"]>(right) Approve["Manager approval"]
  d2<["No"]>(down) space r1<["Approved"]>(down)
  Resolve["Resolve ticket"] r2<["Done"]>(right) Close(("Closed"))

  style Received fill:#696
  style Close fill:#969
```

## 4. Protocol stack (layered composite blocks)

What this shows: three composite blocks stacked vertically to represent a network protocol stack, each with its own internal column layout.

```mermaid
block
  columns 1
  block:App
    columns 3
    HTTP["HTTP/2"] gRPC WebSocket
  end
  block:Transport
    TCP UDP
  end
  block:Network
    columns 1
    IPv4_IPv6["IPv4 / IPv6"]
  end
  App --> Transport --> Network
```
