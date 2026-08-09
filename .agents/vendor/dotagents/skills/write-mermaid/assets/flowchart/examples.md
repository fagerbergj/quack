# Flowchart examples

## CI pipeline with a gated deploy

What this shows: linear stages with a decision gate and a failure branch.

```mermaid
flowchart LR
    A[Push commit] --> B[Run tests]
    B --> C{Tests pass?}
    C -->|Yes| D[Build image]
    C -->|No| E[Notify author]
    D --> F{Approved?}
    F -->|Yes| G[Deploy to prod]
    F -->|No| H[Hold release]
    E --> A
```

## Auth request flow with subgraphs

What this shows: grouping related nodes into subgraphs and linking between the groups.

```mermaid
flowchart TB
    subgraph Client
        A[Login form] --> B[Submit credentials]
    end
    subgraph API
        C[Validate credentials] --> D{Valid?}
        D -->|Yes| E[Issue token]
        D -->|No| F[401 Unauthorized]
    end
    B --> C
    E --> G[Client stores token]
    F --> A
```

## Service dependency graph

What this shows: a many-to-many dependency map using fan-out/fan-in link chaining.

```mermaid
flowchart LR
    Gateway --> Auth & Orders & Billing
    Orders --> Inventory & Billing
    Billing --> PaymentProvider[(Payment Provider)]
    Inventory --> DB[(Postgres)]
```

## Incident response with styled outcome nodes

What this shows: `classDef`/`class` to visually distinguish terminal states.

```mermaid
flowchart TD
    A[Alert fires] --> B[On-call acknowledges]
    B --> C{Root cause found?}
    C -->|Yes| D[Apply fix]
    C -->|No| E[Escalate]
    D --> F[Resolved]
    E --> F
    class F resolved
    classDef resolved fill:#9f9,stroke:#333,stroke-width:2px
```

## Order processing with the v11.3.0+ shape syntax

What this shows: `@{ shape: ... }` for semantically distinct process/decision/database nodes.

```mermaid
flowchart TD
    A@{ shape: rounded, label: "Order placed" }
    B@{ shape: diamond, label: "In stock?" }
    C@{ shape: cyl, label: "Reserve inventory" }
    D@{ shape: dbl-circ, label: "Backorder" }
    A --> B
    B -->|Yes| C
    B -->|No| D
```
